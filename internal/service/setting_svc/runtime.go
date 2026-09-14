package setting_svc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cago-frame/cago/pkg/i18n"
	"github.com/cago-frame/cago/pkg/logger"
	"go.uber.org/zap"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/pkg/code"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
)

// 运行时设置的键。
//
// 这里只有决策 4 划在库这一侧的东西：「进程跑起来之后才生效」的参数。监听地址、
// 数据库 DSN、日志、缓存目录、初始管理密钥留在 config.yaml——数据库连不上时界面
// 本身就不可用，把 DSN 放进库里是个死结。
const (
	// SiteNameSetting 站点名称，界面标题与首页那张名片用。
	SiteNameSetting = "site_name"
	// SiteDomainSetting 站点对外域名，拉取助手拼命令时用。
	SiteDomainSetting = "site_domain"
	// CacheQuotaBytesSetting 缓存总容量上限（字节）。
	CacheQuotaBytesSetting = "cache_quota_bytes"
	// CacheReclaimPercentSetting 回收水位：超配额后一直淘汰到配额的这个百分比。
	CacheReclaimPercentSetting = "cache_reclaim_percent"
	// MutableTTLSecondsSetting 可变对象的默认存活时长（秒）。
	MutableTTLSecondsSetting = "mutable_ttl_seconds"
	// OriginConcurrencySetting 回源并发上限。
	OriginConcurrencySetting = "origin_concurrency"
	// OriginTimeoutSecondsSetting 单次回源的超时（秒）。
	OriginTimeoutSecondsSetting = "origin_timeout_seconds"
	// OriginRetriesSetting 回源失败的重试次数，0 表示不重试。
	OriginRetriesSetting = "origin_retries"
	// RecentRequestRetentionSecondsSetting「最近请求」历史的保留时长（秒）。
	//
	// 它是这份历史占用磁盘的唯一闸门：保留期越长，库越大，占用与流量线性相关。
	RecentRequestRetentionSecondsSetting = "recent_request_retention_seconds"
)

// 出厂默认值，全仓只此一份：缓存层与拉取路径都经 Runtime 读这里，谁也不再自带
// 一套兜底数。两份兜底值意味着「库里没写过这一项」时的行为取决于是谁先问的。
const (
	defaultCacheQuotaBytes      = int64(10) << 30
	defaultCacheReclaimPercent  = 90
	defaultMutableTTLSeconds    = 300
	defaultOriginConcurrency    = 32
	defaultOriginTimeoutSeconds = 30
	defaultOriginRetries        = 2
	// 一天是「刚刚发生了什么」够用、磁盘又吃得消的那个数。
	defaultRecentRequestRetentionSeconds = 86400
)

// settingDef 一项运行时设置的定义：类型、默认值和取值范围。
//
// 校验规则写在定义上而不是散在写入逻辑里，是因为「哪些键存在」和「每个键的合法
// 取值」必须是同一张表回答的——分成两处，加一个键时漏掉校验不会有任何提示。
type settingDef struct {
	Key  string
	Type admin.SettingValueType
	// Default 库里没有这一行时给出的值，也是这一行读不懂时的回退。
	Default json.RawMessage
	// Min、Max 整数项的闭区间。
	Min, Max int64
	// MaxLen 字符串项的长度上限（字节）。
	MaxLen int
}

// settingDefs 全部运行时设置，顺序即界面上的展示顺序。
//
// 管理密钥的哈希不在其中：它虽然同住 setting 表，但读出来对界面毫无用处，
// 只是多一处泄漏面；而能从这里写，就等于绕开轮换端点直接改掉后台凭据。
var settingDefs = []*settingDef{
	{Key: SiteNameSetting, Type: admin.SettingTypeString,
		Default: json.RawMessage(`"katch"`), MaxLen: 64},
	{Key: SiteDomainSetting, Type: admin.SettingTypeString,
		// 253 是一个域名的长度上限，不是随手挑的数。
		Default: json.RawMessage(`""`), MaxLen: 253},
	{Key: PublicHomepageSetting, Type: admin.SettingTypeBool,
		Default: json.RawMessage(`true`)},
	{Key: CacheQuotaBytesSetting, Type: admin.SettingTypeInt,
		Default: jsonInt(defaultCacheQuotaBytes),
		// 配额是一个容量，负数没有意义，0 等于「什么都别缓存」——那不是配额，
		// 是关掉缓存，该有它自己的开关。
		Min: 1, Max: int64(1) << 50},
	{Key: CacheReclaimPercentSetting, Type: admin.SettingTypeInt,
		Default: jsonInt(defaultCacheReclaimPercent),
		// 水位是一个百分比。100 意味着淘汰到刚好等于配额，于是之后每写一个
		// 对象都要再淘汰一次，但那是用户的选择，不是非法值。
		Min: 1, Max: 100},
	{Key: MutableTTLSecondsSetting, Type: admin.SettingTypeInt,
		Default: jsonInt(defaultMutableTTLSeconds),
		// TTL 是一段时长：0 秒的缓存不是缓存，负数更谈不上。上限一天——
		// 可变对象（tag、InRelease）缓存过久就会发出过期内容（决策 7）。
		Min: 1, Max: 86400},
	{Key: OriginConcurrencySetting, Type: admin.SettingTypeInt,
		Default: jsonInt(defaultOriginConcurrency), Min: 1, Max: 4096},
	{Key: OriginTimeoutSecondsSetting, Type: admin.SettingTypeInt,
		Default: jsonInt(defaultOriginTimeoutSeconds), Min: 1, Max: 3600},
	{Key: OriginRetriesSetting, Type: admin.SettingTypeInt,
		// 0 次重试是合法的选择：一次失败就报错，由客户端自己再来。
		Default: jsonInt(defaultOriginRetries), Min: 0, Max: 10},
	{Key: RecentRequestRetentionSecondsSetting, Type: admin.SettingTypeInt,
		// 一份诊断用的历史。下限一小时：比这更短的窗口在界面上看不出任何
		// 东西；上限七天：再长就不再是「最近」，只是把盘占着。
		Default: jsonInt(defaultRecentRequestRetentionSeconds), Min: 3600, Max: 604800},
}

var settingDefIndex = func() map[string]*settingDef {
	index := make(map[string]*settingDef, len(settingDefs))
	for _, def := range settingDefs {
		index[def.Key] = def
	}
	return index
}()

func jsonInt(v int64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf("%d", v))
}

// normalize 校验一个值并给出落库用的 JSON 文本。
//
// 重新编码而不是原样存请求里的字节：请求里的 `0300`、多余空白、超长小数都能表达
// 同一个值，原样存下去会让同一项设置在库里有好几种写法，读的人得先猜是哪一种。
func (d *settingDef) normalize(raw json.RawMessage) (json.RawMessage, error) {
	switch d.Type {
	case admin.SettingTypeBool:
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("应为 true 或 false")
		}
		return jsonMust(v), nil
	case admin.SettingTypeInt:
		var v int64
		// 解进 int64 而不是 float64：小数和带引号的数字都要在这里被挡住，
		// 否则 1.5 会被悄悄截成 1。
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("应为整数")
		}
		if v < d.Min || v > d.Max {
			return nil, fmt.Errorf("应在 %d 到 %d 之间", d.Min, d.Max)
		}
		return jsonInt(v), nil
	case admin.SettingTypeString:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("应为字符串")
		}
		if len(v) > d.MaxLen {
			return nil, fmt.Errorf("长度不能超过 %d 字节", d.MaxLen)
		}
		return jsonMust(v), nil
	}
	return nil, fmt.Errorf("未知的值类型")
}

// jsonMust 编码一个已知能编码的值。bool 与 string 的 Marshal 不会失败，
// 真失败了也只能是运行时坏了，回退成默认的 JSON null 比 panic 温和。
func jsonMust(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}

func (s *settingSvc) List(ctx context.Context, _ *admin.ListSettingsRequest) (*admin.ListSettingsResponse, error) {
	resp := &admin.ListSettingsResponse{List: make([]*admin.SettingItem, 0, len(settingDefs))}
	for _, def := range settingDefs {
		value, err := s.current(ctx, def)
		if err != nil {
			return nil, err
		}
		resp.List = append(resp.List, &admin.SettingItem{
			Key: def.Key, Value: value, Type: def.Type,
		})
	}
	return resp, nil
}

// current 读一项设置此刻的值，没写过或读不懂时给默认值。
//
// 读不懂不报错，和 PublicHomepage 的处理一致：一行坏数据不该让整个设置页打不开，
// 那看起来像是功能丢了，而不是像一处配置错误。但它必须留一条日志——把坏值原样
// 吐出去更糟，那会让整个响应不是合法 JSON。
func (s *settingSvc) current(ctx context.Context, def *settingDef) (json.RawMessage, error) {
	row, err := setting_repo.Setting().Find(ctx, def.Key)
	if err != nil {
		return nil, err
	}
	if row == nil || row.Value == "" {
		return def.Default, nil
	}
	value, err := def.normalize(json.RawMessage(row.Value))
	if err != nil {
		logger.Ctx(ctx).Warn("设置项的值读不懂，按默认值处理",
			zap.String("key", def.Key), zap.String("value", row.Value), zap.Error(err))
		return def.Default, nil
	}
	return value, nil
}

func (s *settingSvc) Save(ctx context.Context, req *admin.SaveSettingsRequest) (*admin.SaveSettingsResponse, error) {
	// 先把整批校验完再写。挑能写的写进去，会在一次「保存失败」之后留下半套设置：
	// 界面刚说没保存成功，库里却已经变了一部分，而且变了哪一部分取决于 map
	// 的遍历顺序，同一个请求重放两次结果还不一样。
	pending := make([]*setting_entity.Setting, 0, len(req.Settings))
	for key, raw := range req.Settings {
		def, ok := settingDefIndex[key]
		if !ok {
			return nil, i18n.NewError(ctx, code.SettingKeyUnknown, key)
		}
		value, err := def.normalize(raw)
		if err != nil {
			return nil, i18n.NewError(ctx, code.SettingValueInvalid, key, err.Error())
		}
		pending = append(pending, &setting_entity.Setting{Key: key, Value: string(value)})
	}
	now := time.Now().Unix()
	for _, row := range pending {
		exist, err := setting_repo.Setting().Find(ctx, row.Key)
		if err != nil {
			return nil, err
		}
		row.Createtime = now
		if exist != nil {
			// 保留原来的创建时间：Save 是整行写回，不带上它这一行会显得像是
			// 每次保存都新建的。
			row.Createtime = exist.Createtime
		}
		row.Updatetime = now
		if err := setting_repo.Setting().Save(ctx, row); err != nil {
			return nil, err
		}
	}
	list, err := s.List(ctx, &admin.ListSettingsRequest{})
	if err != nil {
		return nil, err
	}
	return &admin.SaveSettingsResponse{List: list.List}, nil
}

func (s *settingSvc) RotateAdminKey(ctx context.Context, req *admin.RotateAdminKeyRequest) (*admin.RotateAdminKeyResponse, error) {
	hash, err := hashAdminKey(req.NewKey)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	exist, err := setting_repo.Setting().Find(ctx, AdminKeyHashSetting)
	if err != nil {
		return nil, err
	}
	createtime := now
	if exist != nil {
		createtime = exist.Createtime
	}
	// 写进去就立刻生效：VerifyAdminKey 每次都去读这一行，没有进程内缓存，
	// 所以上一个密钥在下一个请求上就不再被接受了。
	if err := setting_repo.Setting().Save(ctx, &setting_entity.Setting{
		Key:        AdminKeyHashSetting,
		Value:      string(hash),
		Createtime: createtime,
		Updatetime: now,
	}); err != nil {
		return nil, err
	}
	// 不记密钥本身，也不记哈希：日志是会被转走的。
	logger.Ctx(ctx).Info("管理密钥已轮换")
	return &admin.RotateAdminKeyResponse{}, nil
}

// RuntimeSettings 运行时设置此刻的一份取值快照。
//
// 它是一份快照而不是一个长期持有的对象：消费方每次要用的时候读一次，读到的就是
// 库里此刻的值。决策 3/4 把这些项放进库而不是 config.yaml，凭的正是「改完立刻
// 生效、不必重启进程」——把值在构造时抄进某个 service 的字段里，这句话就没了。
type RuntimeSettings struct {
	SiteName   string
	SiteDomain string
	// CacheQuotaBytes 缓存总容量上限（字节）。
	CacheQuotaBytes int64
	// CacheReclaimPercent 回收水位：超配额后一直淘汰到配额的这个百分比。
	CacheReclaimPercent int
	// MutableTTLSeconds 可变对象的默认存活时长，上游没单独配时用它。
	MutableTTLSeconds int64
	// OriginConcurrency 同时压在上游那一侧的回源数上限。
	OriginConcurrency int
	// OriginTimeoutSeconds 单次回源多久拿不到响应算失败。
	OriginTimeoutSeconds int
	// OriginRetries 回源失败重试几次，0 表示不重试。
	OriginRetries int
	// RecentRequestRetentionSeconds「最近请求」历史保留多久（秒）。
	RecentRequestRetentionSeconds int64
}

// RuntimeSource 运行时设置的来源。
//
// 消费方（缓存层、拉取路径）依赖这个接口而不是 SettingSvc 整个：它们要的只是
// 「此刻这几项是多少」，用例注入一个假的也不必去实现密钥轮换。
type RuntimeSource interface {
	Runtime(ctx context.Context) (*RuntimeSettings, error)
}

// defaultRuntimeSettings 出厂值。
//
// 它照着 settingDefs 上的默认值拼，而不是再抄一组常量：抄一份意味着「库里没写过
// 这一项」的行为取决于是谁先问的——设置页读到一个数，拉取路径按另一个数干活。
func defaultRuntimeSettings() *RuntimeSettings {
	rt := &RuntimeSettings{}
	for _, def := range settingDefs {
		// 认领不了的键只会是这份快照不要的那些（public_homepage），
		// 有没有漏掉一项由 TestRuntime_CoversEverySettingDef 守着。
		_ = rt.assign(def.Key, def.Default)
	}
	return rt
}

// Runtime 读出运行时设置此刻的值。
//
// 返回的快照**永远非 nil**，读不出来时给的是出厂值：拉取路径每次回源都要问一次，
// 库坏了就让镜像站停摆，和「缓存是优化，它坏掉不该让拉取整体失败」是同一种错。
// 错误仍旧一并返回，调用方据此记一条日志，管理路径据此报错。
func (s *settingSvc) Runtime(ctx context.Context) (*RuntimeSettings, error) {
	rt := defaultRuntimeSettings()
	if setting_repo.Setting() == nil {
		// 仓储还没装配（main 在起 HTTP 之前就装好了，走到这里的只有那些不碰
		// 设置表的用例）。这不是故障，按出厂值答。
		return rt, nil
	}
	for _, def := range settingDefs {
		value, err := s.current(ctx, def)
		if err != nil {
			return defaultRuntimeSettings(), err
		}
		if err := rt.assign(def.Key, value); err != nil {
			// current 已经把值归一化过，解不出来只可能是这里漏了一个键。
			logger.Ctx(ctx).Warn("运行时设置读不出来，按默认值处理",
				zap.String("key", def.Key), zap.Error(err))
		}
	}
	return rt, nil
}

// assign 把一项归一化之后的值填进快照。
//
// 用一个 switch 而不是反射打 tag：这张表一共八项，switch 漏掉一项时
// TestRuntime_ReadsEverySettingDef 会当场报出来。
func (r *RuntimeSettings) assign(key string, value json.RawMessage) error {
	switch key {
	case SiteNameSetting:
		return json.Unmarshal(value, &r.SiteName)
	case SiteDomainSetting:
		return json.Unmarshal(value, &r.SiteDomain)
	case CacheQuotaBytesSetting:
		return json.Unmarshal(value, &r.CacheQuotaBytes)
	case CacheReclaimPercentSetting:
		return json.Unmarshal(value, &r.CacheReclaimPercent)
	case MutableTTLSecondsSetting:
		return json.Unmarshal(value, &r.MutableTTLSeconds)
	case OriginConcurrencySetting:
		return json.Unmarshal(value, &r.OriginConcurrency)
	case OriginTimeoutSecondsSetting:
		return json.Unmarshal(value, &r.OriginTimeoutSeconds)
	case OriginRetriesSetting:
		return json.Unmarshal(value, &r.OriginRetries)
	case RecentRequestRetentionSecondsSetting:
		return json.Unmarshal(value, &r.RecentRequestRetentionSeconds)
	case PublicHomepageSetting:
		// 首页是否公开有自己的读法（PublicHomepage），不进这份快照：拉取路径
		// 用不上它，而接口层要的是那条「读不出来就收口」的语义。
		return nil
	}
	return fmt.Errorf("没有认领这一项设置")
}
