package gitsmart

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/smartystreets/goconvey/convey"
)

// fixtureRepo 造一个真仓库，返回它的 git 目录与那唯一一条提交的 hash。
//
// 用 go-git 造而不是调 git 二进制：这一层要验的是「katch 发出的字节符合协议」，
// 它不该因为跑用例的机器上没有 git 就红。真 git 客户端认不认这些字节，由
// internal/proxy/extension 那条端到端用例去证。
func fixtureRepo(t *testing.T) (string, plumbing.Hash) {
	t.Helper()
	work := t.TempDir()
	repo, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatalf("建仓库失败：%v", err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("取工作区失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("katch\n"), 0o600); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	if _, err := tree.Add("README.md"); err != nil {
		t.Fatalf("add 失败：%v", err)
	}
	head, err := tree.Commit("初始提交", &git.CommitOptions{Author: &object.Signature{
		Name: "katch", Email: "katch@example.invalid", When: time.Unix(1700000000, 0),
	}})
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	return filepath.Join(work, ".git"), head
}

// TestAdvertise_CarriesServicePreambleAndRefs 目标：本地广播的字节是一份
// smart HTTP 的 ref 广播——带 `# service=` 前导行，并且列出仓库真实的 ref。
func TestAdvertise_CarriesServicePreambleAndRefs(t *testing.T) {
	convey.Convey("本地广播带 service 前导行与真实的 ref", t, func() {
		dir, head := fixtureRepo(t)

		raw, err := Advertise(context.Background(), dir)
		convey.So(err, convey.ShouldBeNil)
		// 前导行必须逐字节对：客户端按 pkt-line 长度前缀读，多一个字节就散架。
		convey.So(string(raw), convey.ShouldStartWith, "001e# service=git-upload-pack\n0000")

		refs := packp.NewAdvRefs()
		convey.So(refs.Decode(bytes.NewReader(raw)), convey.ShouldBeNil)
		convey.So(refs.References["refs/heads/master"], convey.ShouldEqual, head)
		convey.So(refs.Capabilities.Supports(capability.OFSDelta), convey.ShouldBeTrue)
		// shallow 要广播出去：客户端在发 --depth 之前就是看这一项决定能不能发，
		// 而 katch 作为一整台服务器确实答得了它——那一次降级为穿透。
		convey.So(refs.Capabilities.Supports(capability.Shallow), convey.ShouldBeTrue)
	})
}

// TestUploadPack_ServesRequestedCommit 目标：本地应答的 pack 里真的有客户端要的提交。
//
// 判据是把响应里的 packfile 解出来、按 hash 取那个对象，而不是看响应有多长：
// 一个长度对、内容错的 pack 在字节数上看不出任何问题。
func TestUploadPack_ServesRequestedCommit(t *testing.T) {
	convey.Convey("协商的应答里带着客户端 want 的那条提交", t, func() {
		dir, head := fixtureRepo(t)

		body, err := UploadPack(context.Background(), dir, wantRequest(head))
		convey.So(err, convey.ShouldBeNil)
		defer func() { _ = body.Close() }()

		raw, err := io.ReadAll(body)
		convey.So(err, convey.ShouldBeNil)
		// 没有 side-band 时，pack 之前只有一行 NAK。
		convey.So(string(raw), convey.ShouldStartWith, "0008NAK\n")

		store := memory.NewStorage()
		convey.So(packfile.UpdateObjectStorage(store, bytes.NewReader(raw[8:])), convey.ShouldBeNil)
		commit, err := store.EncodedObject(plumbing.CommitObject, head)
		convey.So(err, convey.ShouldBeNil)
		convey.So(commit.Hash(), convey.ShouldEqual, head)
	})
}

// TestUploadPack_ShallowAndFilterAreNotAnswerable 目标 (d)：带 shallow 或 --filter
// 的请求本地答不了，必须让调用方降级为穿透，而不是给客户端一个错误。
//
// go-git 的服务端收到 shallow 直接报错，也完全没有 partial clone；这条降级正是
// `--depth 1` 在本设计里仍然可用的原因（能力边界一节）。
func TestUploadPack_ShallowAndFilterAreNotAnswerable(t *testing.T) {
	convey.Convey("答不了的请求给出 ErrNotAnswerable", t, func() {
		dir, head := fixtureRepo(t)

		cases := map[string][]byte{
			"--depth 1 的 deepen 行":     deepenRequest(head),
			"partial clone 的 filter 行": filterRequest(head),
			"客户端已有的 shallow 边界":        shallowRequest(head),
		}
		for name, req := range cases {
			convey.Convey(name, func() {
				body, err := UploadPack(context.Background(), dir, req)
				convey.So(body, convey.ShouldBeNil)
				convey.So(errors.Is(err, ErrNotAnswerable), convey.ShouldBeTrue)
			})
		}
	})
}

// TestAdvertise_MissingMirrorIsNotAnswerable 盘上没有这份镜像时同样是降级，
// 而不是一个把 clone 打断的错误——库里说 ready 而目录被人删了，是会发生的。
func TestAdvertise_MissingMirrorIsNotAnswerable(t *testing.T) {
	convey.Convey("镜像目录不在时给出 ErrNotAnswerable", t, func() {
		_, err := Advertise(context.Background(), filepath.Join(t.TempDir(), "gone.git"))
		convey.So(errors.Is(err, ErrNotAnswerable), convey.ShouldBeTrue)
	})
}

// wantRequest 一次最朴素的协商请求：要一条提交、什么都没有、说完。
func wantRequest(want plumbing.Hash) []byte {
	var buf bytes.Buffer
	enc := pktline.NewEncoder(&buf)
	_ = enc.Encodef("want %s ofs-delta agent=git/katch-test\n", want)
	_ = enc.Flush()
	_ = enc.EncodeString("done\n")
	return buf.Bytes()
}

// deepenRequest 带 deepen 行的请求，git clone --depth 1 发的就是这个形态。
func deepenRequest(want plumbing.Hash) []byte {
	var buf bytes.Buffer
	enc := pktline.NewEncoder(&buf)
	_ = enc.Encodef("want %s ofs-delta agent=git/katch-test\n", want)
	_ = enc.EncodeString("deepen 1\n")
	_ = enc.Flush()
	_ = enc.EncodeString("done\n")
	return buf.Bytes()
}

// filterRequest 带 filter 行的请求，git clone --filter=blob:none 发的就是这个形态。
func filterRequest(want plumbing.Hash) []byte {
	var buf bytes.Buffer
	enc := pktline.NewEncoder(&buf)
	_ = enc.Encodef("want %s ofs-delta agent=git/katch-test\n", want)
	_ = enc.EncodeString("filter blob:none\n")
	_ = enc.Flush()
	_ = enc.EncodeString("done\n")
	return buf.Bytes()
}

// shallowRequest 客户端手上已经是一份浅仓库时发的形态。
func shallowRequest(want plumbing.Hash) []byte {
	var buf bytes.Buffer
	enc := pktline.NewEncoder(&buf)
	_ = enc.Encodef("want %s ofs-delta agent=git/katch-test\n", want)
	_ = enc.Encodef("shallow %s\n", want)
	_ = enc.Flush()
	_ = enc.EncodeString("done\n")
	return buf.Bytes()
}
