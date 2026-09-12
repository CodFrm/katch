// Package setting_svc 是运行时设置的业务层，眼下只承担管理密钥。
package setting_svc

import (
	"context"
	"strconv"
	"time"

	"github.com/cago-frame/cago/pkg/i18n"
	"github.com/cago-frame/cago/pkg/logger"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/pkg/code"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
)

// AdminKeyHashSetting 管理密钥哈希在 setting 表里的键。
//
// 存哈希而不是明文：这张表在界面上可读，明文密钥落在一个能被列出来的键值表里，
// 等于把密钥摊开给任何一个已经进了后台的人。
const AdminKeyHashSetting = "admin_key_hash"

// PublicHomepageSetting 「首页是否公开上游列表与命中率」在 setting 表里的键。
//
// 值是 JSON 的 true / false，和这张表其余的值一样（「数据模型」一节）。
const PublicHomepageSetting = "public_homepage"

// SettingSvc 运行时设置的业务操作。
type SettingSvc interface {
	// VerifyAdminKey 校验管理密钥。密钥错误与未提供密钥返回的是同一个错误，
	// 调用方无从区分——区分开就等于告诉探测者「字段找对了，只是值不对」。
	VerifyAdminKey(ctx context.Context, key string) error
	// EnsureAdminKey 在库中尚无密钥时，用 config.yaml 给的初始密钥落一份哈希。
	// 库里已经有密钥时它什么也不做：轮换发生在界面上，改配置文件不应该把它覆盖回去。
	EnsureAdminKey(ctx context.Context, initialKey string) error
	// PublicHomepage 首页的上游列表与命中率是否对匿名调用方可见。
	// 库里没有这一行、或者这一行的值读不懂时都按可见处理，理由见实现。
	PublicHomepage(ctx context.Context) (bool, error)
}

type settingSvc struct{}

var defaultSetting = &settingSvc{}

// Setting 返回设置业务层。
func Setting() SettingSvc {
	return defaultSetting
}

func (s *settingSvc) VerifyAdminKey(ctx context.Context, key string) error {
	setting, err := setting_repo.Setting().Find(ctx, AdminKeyHashSetting)
	if err != nil {
		return err
	}
	if setting == nil || setting.Value == "" {
		// 库里没有密钥时一律拒绝。这不是「放行」的降级：没有密钥的实例如果默认敞开，
		// 一次失败的初始化就会变成一个任何人都能改规则的后台。
		return i18n.NewUnauthorizedError(ctx, code.AdminKeyNotInitialized)
	}
	// 即使 key 为空也照样比一次 bcrypt：省掉这一步会让「没带密钥」比「密钥错误」
	// 快上一个数量级，响应体一致但耗时不一致，同样是一个可区分的信号。
	if err := bcrypt.CompareHashAndPassword([]byte(setting.Value), []byte(key)); err != nil {
		return i18n.NewUnauthorizedError(ctx, code.AdminKeyInvalid)
	}
	return nil
}

func (s *settingSvc) EnsureAdminKey(ctx context.Context, initialKey string) error {
	setting, err := setting_repo.Setting().Find(ctx, AdminKeyHashSetting)
	if err != nil {
		return err
	}
	if setting != nil && setting.Value != "" {
		return nil
	}
	if initialKey == "" {
		// 没有初始密钥也不是启动失败：管理接口会全量 401，拉取路径照常服务。
		// 把这当成致命错误会让一个纯拉取用途的部署起不来。
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(initialKey), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	return setting_repo.Setting().Save(ctx, &setting_entity.Setting{
		Key:        AdminKeyHashSetting,
		Value:      string(hash),
		Createtime: now,
		Updatetime: now,
	})
}

func (s *settingSvc) PublicHomepage(ctx context.Context) (bool, error) {
	setting, err := setting_repo.Setting().Find(ctx, PublicHomepageSetting)
	if err != nil {
		// 不在这里兜底成 true：读不出设置和「设置说公开」是两回事，
		// 调用方要能区分「站长关掉了」和「这次判不了」。
		return false, err
	}
	// 默认可见。一台镜像站的价值就在于答得出自己代理了什么，默认藏起来会让
	// 刚装好的实例首页空着，而部署者根本不知道有个开关要打开。存坏了的值同理：
	// 那看起来会像是功能丢了，而不是像一处配置错误。
	if setting == nil {
		return true, nil
	}
	public, err := strconv.ParseBool(setting.Value)
	if err != nil {
		logger.Ctx(ctx).Warn("公开首页设置的值读不懂，按公开处理",
			zap.String("value", setting.Value))
		return true, nil
	}
	return public, nil
}
