package setting_svc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/smartystreets/goconvey/convey"

	"github.com/CodFrm/katch/internal/repository/setting_repo"
)

// TestGitRuntimeSettings git 镜像的六项运行时设置：读得出、写得进、写完立刻生效。
//
// 「立刻生效」在这一层的判据就是「下一次 Runtime 读到的是新值」——消费方
// （建镜像那一侧）每次用到时现读这份快照，不在构造时抄进字段里（决策 3/4）。
func TestGitRuntimeSettings(t *testing.T) {
	convey.Convey("git 镜像的运行时设置", t, func() {
		repo := newMemorySettingRepo()
		setting_repo.RegisterSetting(repo)
		ctx := context.Background()

		convey.Convey("没写过时给出厂值", func() {
			rt, err := Setting().Runtime(ctx)
			convey.So(err, convey.ShouldBeNil)
			convey.So(rt.GitMirrorQuotaBytes, convey.ShouldEqual, defaultGitMirrorQuotaBytes)
			convey.So(rt.GitRepoMaxBytes, convey.ShouldEqual, defaultGitRepoMaxBytes)
			convey.So(rt.GitSyncTimeoutSeconds, convey.ShouldEqual, defaultGitSyncTimeoutSeconds)
			convey.So(rt.GitSyncConcurrency, convey.ShouldEqual, defaultGitSyncConcurrency)
			convey.So(rt.GitBuildStallSeconds, convey.ShouldEqual, defaultGitBuildStallSeconds)
			convey.So(rt.GitBuildTimeoutSeconds, convey.ShouldEqual, defaultGitBuildTimeoutSeconds)
		})

		convey.Convey("六项都在设置列表里，界面才有得改", func() {
			// 设置页读的就是 List：不在这张表里的键，界面既读不到也存不进去。
			list, err := Setting().List(ctx, nil)
			convey.So(err, convey.ShouldBeNil)
			keys := map[string]bool{}
			for _, item := range list.List {
				keys[item.Key] = true
			}
			convey.So(keys[GitMirrorQuotaBytesSetting], convey.ShouldBeTrue)
			convey.So(keys[GitRepoMaxBytesSetting], convey.ShouldBeTrue)
			convey.So(keys[GitSyncTimeoutSecondsSetting], convey.ShouldBeTrue)
			convey.So(keys[GitSyncConcurrencySetting], convey.ShouldBeTrue)
			convey.So(keys[GitBuildStallSecondsSetting], convey.ShouldBeTrue)
			convey.So(keys[GitBuildTimeoutSecondsSetting], convey.ShouldBeTrue)
		})

		convey.Convey("写进去的值，下一次读就是新的，不必重启", func() {
			_, err := Setting().Save(ctx, saveRequest(map[string]json.RawMessage{
				GitMirrorQuotaBytesSetting:    json.RawMessage(`8192`),
				GitRepoMaxBytesSetting:        json.RawMessage(`4096`),
				GitSyncTimeoutSecondsSetting:  json.RawMessage(`11`),
				GitSyncConcurrencySetting:     json.RawMessage(`3`),
				GitBuildStallSecondsSetting:   json.RawMessage(`45`),
				GitBuildTimeoutSecondsSetting: json.RawMessage(`900`),
			}))
			convey.So(err, convey.ShouldBeNil)

			rt, err := Setting().Runtime(ctx)
			convey.So(err, convey.ShouldBeNil)
			convey.So(rt.GitMirrorQuotaBytes, convey.ShouldEqual, 8192)
			convey.So(rt.GitRepoMaxBytes, convey.ShouldEqual, 4096)
			convey.So(rt.GitSyncTimeoutSeconds, convey.ShouldEqual, 11)
			convey.So(rt.GitSyncConcurrency, convey.ShouldEqual, 3)
			convey.So(rt.GitBuildStallSeconds, convey.ShouldEqual, 45)
			convey.So(rt.GitBuildTimeoutSeconds, convey.ShouldEqual, 900)
		})

		convey.Convey("不合法的取值存不进去", func() {
			// 0 字节的单仓上限等于「任何仓库都不许镜像」，那不是一个上限，
			// 是一个开关；0 并发同理，会让后台建镜像这件事永远排不上队。
			for key, bad := range map[string]json.RawMessage{
				GitRepoMaxBytesSetting:       json.RawMessage(`0`),
				GitSyncConcurrencySetting:    json.RawMessage(`0`),
				GitSyncTimeoutSecondsSetting: json.RawMessage(`-1`),
				GitMirrorQuotaBytesSetting:   json.RawMessage(`0`),
				// 0 秒的停滞时限会让任何一次建镜像在第一个字节之前就被判死；
				// 总时限同理，而且它还要挡住「大到没边」的那一头。
				GitBuildStallSecondsSetting:   json.RawMessage(`0`),
				GitBuildTimeoutSecondsSetting: json.RawMessage(`86401`),
			} {
				_, err := Setting().Save(ctx, saveRequest(map[string]json.RawMessage{key: bad}))
				convey.So(err, convey.ShouldNotBeNil)
			}
			// 一条都没存进去：写不进的那一批不该在库里留下半套。
			rt, err := Setting().Runtime(ctx)
			convey.So(err, convey.ShouldBeNil)
			convey.So(rt.GitRepoMaxBytes, convey.ShouldEqual, defaultGitRepoMaxBytes)
			convey.So(rt.GitSyncConcurrency, convey.ShouldEqual, defaultGitSyncConcurrency)
			convey.So(rt.GitBuildStallSeconds, convey.ShouldEqual, defaultGitBuildStallSeconds)
			convey.So(rt.GitBuildTimeoutSeconds, convey.ShouldEqual, defaultGitBuildTimeoutSeconds)
		})
	})
}
