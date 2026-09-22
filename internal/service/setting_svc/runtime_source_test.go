package setting_svc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"go.uber.org/mock/gomock"

	"github.com/CodFrm/katch/internal/api/admin"
	"github.com/CodFrm/katch/internal/api/site"
	"github.com/CodFrm/katch/internal/model/entity/setting_entity"
	"github.com/CodFrm/katch/internal/repository/setting_repo"
	mock_setting_repo "github.com/CodFrm/katch/internal/repository/setting_repo/mock"
	"github.com/CodFrm/katch/internal/repository/upstream_repo"
)

// memorySettingRepo 一张背在内存里的设置表，用来数「库被问了几次」。
//
// 逐次 EXPECT 写不出这条断言：要验的是同一个键被问了一次还是每次请求都问一次，
// 那是一个计数，不是一串固定的调用序列。
type memorySettingRepo struct {
	mu sync.Mutex
	// finds 每个键被真正查了几次库。
	finds map[string]int
	rows  map[string]*setting_entity.Setting
}

type memoryRewriteConfigRepo struct {
	advances int
}

func (m *memoryRewriteConfigRepo) Transaction(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

func (m *memoryRewriteConfigRepo) AdvanceGeneration(context.Context) error {
	m.advances++
	return nil
}

func (m *memoryRewriteConfigRepo) Snapshot(context.Context) (*upstream_repo.RewriteConfigSnapshot, error) {
	return &upstream_repo.RewriteConfigSnapshot{}, nil
}

func newMemorySettingRepo() *memorySettingRepo {
	upstream_repo.RegisterRewriteConfig(&memoryRewriteConfigRepo{})
	return &memorySettingRepo{finds: map[string]int{}, rows: map[string]*setting_entity.Setting{}}
}

func (m *memorySettingRepo) Find(_ context.Context, key string) (*setting_entity.Setting, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finds[key]++
	row, ok := m.rows[key]
	if !ok {
		return nil, nil
	}
	copied := *row
	return &copied, nil
}

func (m *memorySettingRepo) Save(_ context.Context, setting *setting_entity.Setting) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := *setting
	m.rows[setting.Key] = &copied
	return nil
}

func (m *memorySettingRepo) findCount(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.finds[key]
}

type settingTxContextKey struct{}

type commitAwareSettingRepo struct {
	mu             sync.Mutex
	committed      map[string]*setting_entity.Setting
	staged         map[string]*setting_entity.Setting
	committedFinds int
}

func cloneSettingRows(src map[string]*setting_entity.Setting) map[string]*setting_entity.Setting {
	dst := make(map[string]*setting_entity.Setting, len(src))
	for key, row := range src {
		copied := *row
		dst[key] = &copied
	}
	return dst
}

func (r *commitAwareSettingRepo) begin() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.staged = cloneSettingRows(r.committed)
}

func (r *commitAwareSettingRepo) finish(commit bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if commit {
		r.committed = cloneSettingRows(r.staged)
	}
	r.staged = nil
}

func (r *commitAwareSettingRepo) rows(ctx context.Context) map[string]*setting_entity.Setting {
	if inTx, _ := ctx.Value(settingTxContextKey{}).(bool); inTx {
		return r.staged
	}
	return r.committed
}

func (r *commitAwareSettingRepo) Find(ctx context.Context, key string) (*setting_entity.Setting, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if inTx, _ := ctx.Value(settingTxContextKey{}).(bool); !inTx {
		r.committedFinds++
	}
	row := r.rows(ctx)[key]
	if row == nil {
		return nil, nil
	}
	copied := *row
	return &copied, nil
}

func (r *commitAwareSettingRepo) Save(ctx context.Context, row *setting_entity.Setting) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if inTx, _ := ctx.Value(settingTxContextKey{}).(bool); !inTx {
		return errors.New("test write escaped transaction")
	}
	copied := *row
	r.staged[row.Key] = &copied
	return nil
}

type barrierSettingRewriteRepo struct {
	settings     *commitAwareSettingRepo
	callbackDone chan struct{}
	finish       chan struct{}
	rollback     bool
}

func (r *barrierSettingRewriteRepo) Transaction(ctx context.Context, fn func(context.Context) error) error {
	r.settings.begin()
	err := fn(context.WithValue(ctx, settingTxContextKey{}, true))
	close(r.callbackDone)
	<-r.finish
	if err != nil || r.rollback {
		r.settings.finish(false)
		if err != nil {
			return err
		}
		return errors.New("forced rollback")
	}
	r.settings.finish(true)
	return nil
}

func (r *barrierSettingRewriteRepo) AdvanceGeneration(context.Context) error { return nil }

func (r *barrierSettingRewriteRepo) Snapshot(context.Context) (*upstream_repo.RewriteConfigSnapshot, error) {
	return &upstream_repo.RewriteConfigSnapshot{}, nil
}

// TestRuntime_FallsBackToDefaults 库里一条都没写过时，读到的是出厂值。
func TestRuntime_FallsBackToDefaults(t *testing.T) {
	convey.Convey("没写过的运行时项给出厂值", t, func() {
		repo := setupSettingTest(t)
		repo.EXPECT().Find(gomock.Any(), gomock.Any()).AnyTimes().Return(nil, nil)

		rt, err := Setting().Runtime(context.Background())
		convey.So(err, convey.ShouldBeNil)
		convey.So(rt.SiteName, convey.ShouldEqual, "katch")
		convey.So(rt.SiteDomain, convey.ShouldEqual, "")
		convey.So(rt.CacheQuotaBytes, convey.ShouldEqual, defaultCacheQuotaBytes)
		convey.So(rt.CacheReclaimPercent, convey.ShouldEqual, defaultCacheReclaimPercent)
		convey.So(rt.MutableTTLSeconds, convey.ShouldEqual, defaultMutableTTLSeconds)
		convey.So(rt.OriginConcurrency, convey.ShouldEqual, defaultOriginConcurrency)
		convey.So(rt.OriginTimeoutSeconds, convey.ShouldEqual, defaultOriginTimeoutSeconds)
		convey.So(rt.OriginRetries, convey.ShouldEqual, defaultOriginRetries)
		convey.So(rt.RecentRequestRetentionSeconds, convey.ShouldEqual, int64(86400))
	})
}

// TestRuntime_ReadsWhatWasSaved 写进去的值，下一次读就是新的。
func TestRuntime_ReadsWhatWasSaved(t *testing.T) {
	convey.Convey("经 Save 写进去的运行时项，下一次读就生效", t, func() {
		repo := newMemorySettingRepo()
		setting_repo.RegisterSetting(repo)
		ctx := context.Background()

		_, err := Setting().Save(ctx, saveRequest(map[string]json.RawMessage{
			CacheQuotaBytesSetting:      json.RawMessage(`4096`),
			OriginConcurrencySetting:    json.RawMessage(`3`),
			OriginTimeoutSecondsSetting: json.RawMessage(`7`),
			OriginRetriesSetting:        json.RawMessage(`0`),
			SiteDomainSetting:           json.RawMessage(`"mirror.example.com"`),
		}))
		convey.So(err, convey.ShouldBeNil)

		rt, err := Setting().Runtime(ctx)
		convey.So(err, convey.ShouldBeNil)
		convey.So(rt.CacheQuotaBytes, convey.ShouldEqual, 4096)
		convey.So(rt.OriginConcurrency, convey.ShouldEqual, 3)
		convey.So(rt.OriginTimeoutSeconds, convey.ShouldEqual, 7)
		convey.So(rt.OriginRetries, convey.ShouldEqual, 0)
		convey.So(rt.SiteDomain, convey.ShouldEqual, "mirror.example.com")
		// 没写的那几项还是出厂值，不会被这一次保存带跑。
		convey.So(rt.CacheReclaimPercent, convey.ShouldEqual, defaultCacheReclaimPercent)
	})
}

// TestRuntime_KeepsServingOnRepoError 库读不出来时给出厂值并把错误交出去。
//
// 这是「失败与降级」那条：库不可用时读路径要能继续服务。返回 nil 快照会让调用方
// 在拉取路径上当场 panic，比停摆还糟。
func TestRuntime_KeepsServingOnRepoError(t *testing.T) {
	convey.Convey("设置表读不出来时给出厂值，错误一并交出去", t, func() {
		repo := setupSettingTest(t)
		repo.EXPECT().Find(gomock.Any(), gomock.Any()).AnyTimes().
			Return(nil, errors.New("库挂了"))

		rt, err := Setting().Runtime(context.Background())
		convey.So(err, convey.ShouldNotBeNil)
		convey.So(rt, convey.ShouldNotBeNil)
		convey.So(rt.OriginTimeoutSeconds, convey.ShouldEqual, defaultOriginTimeoutSeconds)
	})
}

// TestRuntime_CoversEverySettingDef 每一项运行时设置都要在快照里有个落点。
//
// 少了这条，往 settingDefs 里加一个键而忘了在 assign 里认领它，只会在运行时
// 悄悄退回默认值——界面上改得动、存得下，就是不生效。
func TestRuntime_CoversEverySettingDef(t *testing.T) {
	convey.Convey("settingDefs 里的每一项都被 RuntimeSettings 认领", t, func() {
		for _, def := range settingDefs {
			rt := &RuntimeSettings{}
			convey.So(rt.assign(def.Key, def.Default), convey.ShouldBeNil)
		}
	})
}

func TestSiteDomainAdvancesRewriteGenerationOnlyWhenChanged(t *testing.T) {
	convey.Convey("site_domain 与 rewrite generation 同一次保存生效", t, func() {
		repo := newMemorySettingRepo()
		setting_repo.RegisterSetting(repo)
		rewrite := &memoryRewriteConfigRepo{}
		upstream_repo.RegisterRewriteConfig(rewrite)
		ctx := context.Background()

		save := func(domain string) {
			_, err := Setting().Save(ctx, saveRequest(map[string]json.RawMessage{
				SiteDomainSetting: mustJSON(domain),
			}))
			convey.So(err, convey.ShouldBeNil)
		}
		save("mirror.example.com")
		convey.So(rewrite.advances, convey.ShouldEqual, 1)
		save("mirror.example.com")
		convey.So(rewrite.advances, convey.ShouldEqual, 1)
		save("new.example.com")
		convey.So(rewrite.advances, convey.ShouldEqual, 2)
	})
}

// TestSiteDomainSave_InvalidatesOnlyAfterCommit reproduces the setting-cache
// variant of the pre-commit invalidation race with transaction barriers.
func TestSiteDomainSave_InvalidatesOnlyAfterCommit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rollback bool
	}{
		{name: "commit"},
		{name: "rollback", rollback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := &commitAwareSettingRepo{committed: map[string]*setting_entity.Setting{
				SiteDomainSetting: {Key: SiteDomainSetting, Value: `"old.example"`},
			}}
			rewrite := &barrierSettingRewriteRepo{
				settings: inner, callbackDone: make(chan struct{}), finish: make(chan struct{}), rollback: tc.rollback,
			}
			previousSetting := setting_repo.Setting()
			previousRewrite := upstream_repo.RewriteConfig()
			setting_repo.RegisterSetting(NewCachedSettingRepo(inner))
			upstream_repo.RegisterRewriteConfig(rewrite)
			t.Cleanup(func() {
				setting_repo.RegisterSetting(previousSetting)
				upstream_repo.RegisterRewriteConfig(previousRewrite)
			})

			ctx := context.Background()
			primed, err := Setting().BaseURL(ctx)
			if err != nil || primed != "https://old.example" {
				t.Fatalf("prime site domain: got %q, err %v", primed, err)
			}

			writeDone := make(chan error, 1)
			go func() {
				_, err := Setting().Save(ctx, saveRequest(map[string]json.RawMessage{
					SiteDomainSetting: json.RawMessage(`"new.example"`),
				}))
				writeDone <- err
			}()
			<-rewrite.callbackDone

			during, err := Setting().BaseURL(ctx)
			if err != nil || during != "https://old.example" {
				t.Fatalf("uncommitted site domain became visible: got %q, err %v", during, err)
			}
			close(rewrite.finish)
			err = <-writeDone

			after, readErr := Setting().BaseURL(ctx)
			if tc.rollback {
				if err == nil || readErr != nil || after != "https://old.example" {
					t.Fatalf("rollback changed visible domain: write err %v, got %q, read err %v", err, after, readErr)
				}
				if inner.committedFinds != 1 {
					t.Fatalf("rollback invalidated valid cache: committed Find called %d times, want 1", inner.committedFinds)
				}
				return
			}
			if err != nil || readErr != nil || after != "https://new.example" {
				t.Fatalf("committed site domain not visible: write err %v, got %q, read err %v", err, after, readErr)
			}
		})
	}
}

// TestCachedSettingRepo_AnswersFromMemory 进程内缓存：同一个键不该每次都查库。
func TestCachedSettingRepo_AnswersFromMemory(t *testing.T) {
	convey.Convey("设置表包上进程内缓存之后", t, func() {
		inner := newMemorySettingRepo()
		cached := NewCachedSettingRepo(inner)
		ctx := context.Background()

		convey.Convey("写过的键只查一次库，其余请求从内存答", func() {
			convey.So(cached.Save(ctx, &setting_entity.Setting{
				Key: CacheQuotaBytesSetting, Value: "4096",
			}), convey.ShouldBeNil)
			for range 5 {
				row, err := cached.Find(ctx, CacheQuotaBytesSetting)
				convey.So(err, convey.ShouldBeNil)
				convey.So(row.Value, convey.ShouldEqual, "4096")
			}
			convey.So(inner.findCount(CacheQuotaBytesSetting), convey.ShouldEqual, 1)
		})

		convey.Convey("库里根本没有的键也只查一次", func() {
			// 没写过的项是常态（出厂就没有一行），不把「没有」记下来，
			// 这些键就会每个请求查一次库——正是这层缓存要挡掉的那件事。
			for range 5 {
				row, err := cached.Find(ctx, SiteDomainSetting)
				convey.So(err, convey.ShouldBeNil)
				convey.So(row, convey.ShouldBeNil)
			}
			convey.So(inner.findCount(SiteDomainSetting), convey.ShouldEqual, 1)
		})

		convey.Convey("写入之后缓存失效，下一次读到的是新值", func() {
			_, err := cached.Find(ctx, SiteDomainSetting)
			convey.So(err, convey.ShouldBeNil)
			convey.So(cached.Save(ctx, &setting_entity.Setting{
				Key: SiteDomainSetting, Value: `"mirror.example.com"`,
			}), convey.ShouldBeNil)

			row, err := cached.Find(ctx, SiteDomainSetting)
			convey.So(err, convey.ShouldBeNil)
			convey.So(row.Value, convey.ShouldEqual, `"mirror.example.com"`)
		})

		convey.Convey("写库失败时不动缓存", func() {
			broken := mock_setting_repo.NewMockSettingRepo(gomock.NewController(t))
			broken.EXPECT().Find(gomock.Any(), SiteNameSetting).Times(1).
				Return(&setting_entity.Setting{Key: SiteNameSetting, Value: `"旧名字"`}, nil)
			broken.EXPECT().Save(gomock.Any(), gomock.Any()).Return(errors.New("库挂了"))
			repo := NewCachedSettingRepo(broken)

			_, err := repo.Find(ctx, SiteNameSetting)
			convey.So(err, convey.ShouldBeNil)
			convey.So(repo.Save(ctx, &setting_entity.Setting{
				Key: SiteNameSetting, Value: `"新名字"`,
			}), convey.ShouldNotBeNil)
			// 库里没变，缓存就还是对的：这一次读不该再去问库。
			row, err := repo.Find(ctx, SiteNameSetting)
			convey.So(err, convey.ShouldBeNil)
			convey.So(row.Value, convey.ShouldEqual, `"旧名字"`)
		})
	})
}

// TestSiteInfo_BuildsBaseURLFromDomain 站点地址是拉取命令的第一段，
// 它由设置里的域名拼出来。
func TestSiteInfo_BuildsBaseURLFromDomain(t *testing.T) {
	convey.Convey("站点名片按设置里的域名给出命令用的地址", t, func() {
		repo := newMemorySettingRepo()
		setting_repo.RegisterSetting(repo)
		ctx := context.Background()

		convey.Convey("没配域名时不替调用方猜一个", func() {
			info, err := Setting().SiteInfo(ctx, &site.InfoRequest{})
			convey.So(err, convey.ShouldBeNil)
			convey.So(info.Name, convey.ShouldEqual, "katch")
			convey.So(info.BaseURL, convey.ShouldEqual, "")
		})

		convey.Convey("裸域名补 https，带协议的原样留下，尾部斜杠一律去掉", func() {
			cases := []struct{ domain, want string }{
				{"mirror.example.com", "https://mirror.example.com"},
				{"mirror.example.com/", "https://mirror.example.com"},
				{" mirror.example.com ", "https://mirror.example.com"},
				{"http://10.0.0.2:8080", "http://10.0.0.2:8080"},
				{"https://mirror.example.com/", "https://mirror.example.com"},
			}
			for _, c := range cases {
				_, err := Setting().Save(ctx, saveRequest(map[string]json.RawMessage{
					SiteDomainSetting: mustJSON(c.domain),
				}))
				convey.So(err, convey.ShouldBeNil)
				info, err := Setting().SiteInfo(ctx, &site.InfoRequest{})
				convey.So(err, convey.ShouldBeNil)
				convey.So(info.BaseURL, convey.ShouldEqual, c.want)
			}
		})
	})
}

// saveRequest 拼一次设置保存请求。
func saveRequest(settings map[string]json.RawMessage) *admin.SaveSettingsRequest {
	return &admin.SaveSettingsRequest{Settings: settings}
}

func mustJSON(v string) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
