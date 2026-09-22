package bootstrap

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/smartystreets/goconvey/convey"
	"gopkg.in/yaml.v3"
)

// TestConfigExample_KeepsTheErrorLogWorthReading 发出去的示例配置不能把错误日志变成 SQL 噪音。
//
// 示例文件自己写着「单独收 error 及以上级别，值班时只看这一个文件即可」。可 cago 把
// **顶层** debug 传给 gorm 的日志器（database/db/db.go 里 newDB(cfg, config.Debug)），
// 打开时 IgnoreRecordNotFoundError 变成 false、Colorful 变成 true：于是每一次缓存未命中
// 查 cache_object、每一次启动查 setting 都会以 ERROR 级别、带 ANSI 转义写进那个文件。
// 真机上验出来的样子是 err 日志 100% 是 record not found——值班时该看的那一个文件里
// 没有一行是真错误。
//
// db 段里那个 debug 管的是别的事（cago 用它决定要不要 orm.Debug() 打印每条 SQL），
// 关掉它并不会让顶层这一个失效，所以这条守的是顶层那个。
func TestConfigExample_KeepsTheErrorLogWorthReading(t *testing.T) {
	convey.Convey("示例配置不打开全局 debug", t, func() {
		_, thisFile, _, ok := runtime.Caller(0)
		convey.So(ok, convey.ShouldBeTrue)
		root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
		// 两份都守：configs/ 那份是本机与 smoke 用的，deploy/ 那份是部署照抄的。
		for _, relative := range [][]string{{"configs", "config.yaml.example"}, {"deploy", "config.yaml.example"}} {
			raw, err := os.ReadFile(filepath.Join(root, filepath.Join(relative...)))
			convey.So(err, convey.ShouldBeNil)
			assertQuietErrorLog(raw)
		}
	})
}

func assertQuietErrorLog(raw []byte) {
	var example struct {
		Debug  bool `yaml:"debug"`
		Logger struct {
			LogFile struct {
				Enable        bool   `yaml:"enable"`
				ErrorFilename string `yaml:"errorFilename"`
			} `yaml:"logFile"`
		} `yaml:"logger"`
	}
	convey.So(yaml.Unmarshal(raw, &example), convey.ShouldBeNil)

	// 先确认这条守的前提还在：错误日志仍然是单独一个文件。
	convey.So(example.Logger.LogFile.Enable, convey.ShouldBeTrue)
	convey.So(example.Logger.LogFile.ErrorFilename, convey.ShouldNotBeEmpty)
	convey.So(example.Debug, convey.ShouldBeFalse)
}

// TestConfigExample_SQLiteDSNKeepsCommitsCheap 随仓库发出的每一份 sqlite DSN 都带齐三个 pragma。
//
// synchronous(NORMAL) 是真机上查出来的：WAL 下默认的 FULL 让每次提交都 fsync 一次，而缓存
// 本身正往同一块盘上写大文件，一次 fsync 要几百毫秒到几秒。coding.local 上复现的样子是
// 16 个并发写最长 5.08 秒、还有两次等锁超过 busy_timeout 直接报 database is locked——落在
// 缓存记录上就是这份字节白下了、盘上多一个没人认领的文件；落在命中路径的 Touch 上就是
// 客户端干等。换成 NORMAL 后同样的负载最长 141 毫秒，一次都没超过 200 毫秒。
//
// NORMAL 在 WAL 下只在断电或系统崩溃时可能丢掉最后几个事务（进程崩溃不丢），对缓存元数据
// 来说代价是下一次重新回源。WAL 与 busy_timeout 为什么不能省，见示例配置里的注释。
func TestConfigExample_SQLiteDSNKeepsCommitsCheap(t *testing.T) {
	convey.Convey("每一份发出去的 sqlite DSN 都带 WAL、busy_timeout 与 synchronous(NORMAL)", t, func() {
		_, thisFile, _, ok := runtime.Caller(0)
		convey.So(ok, convey.ShouldBeTrue)
		root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
		for _, relative := range []string{
			"configs/config.yaml.example",
			"deploy/config.yaml.example",
			"deploy/kubernetes/secret.yaml",
			"deploy/helm/katch/values.yaml",
		} {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
			convey.So(err, convey.ShouldBeNil)
			found := 0
			for _, line := range strings.Split(string(raw), "\n") {
				trimmed := strings.TrimSpace(line)
				if !strings.HasPrefix(trimmed, "dsn:") || !strings.Contains(trimmed, "file:") {
					continue
				}
				found++
				for _, pragma := range []string{
					"_pragma=journal_mode(WAL)", "_pragma=busy_timeout(5000)", "_pragma=synchronous(NORMAL)",
				} {
					convey.So(relative+": "+trimmed, convey.ShouldContainSubstring, pragma)
				}
			}
			// 先确认这条守的前提还在：文件里确实有一条 sqlite DSN，不然上面的循环什么都没验。
			convey.So(found, convey.ShouldBeGreaterThan, 0)
		}
	})
}
