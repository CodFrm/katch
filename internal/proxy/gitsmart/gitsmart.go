// Package gitsmart 是 smart HTTP 与一份盘上裸仓库之间的胶水层。
//
// go-git 有服务端的会话实现，但没有 HTTP 那一层：`# service=` 前导行、请求体
// 怎么切、响应用哪个 media type，都得 katch 自己写（能力边界一节）。这个包就是
// 那几十行胶水，除此之外不做任何判断——镜像在不在、新不新鲜、该不该本地答，
// 全是 git_svc 的事。
//
// 它能答什么由 go-git v5 决定（源码核实）：广播的能力集只有 agent 与 ofs-delta，
// 没有协议 v2；shallow 收到就报错；没有 partial clone。因此带 shallow / deepen /
// filter 的请求一律给回 ErrNotAnswerable，由调用方降级为穿透——那条降级正是
// `git clone --depth 1` 在本设计里仍然可用的唯一原因。
package gitsmart

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// smart HTTP 的三个 media type，按协议原样写死。
const (
	// AdvertisementContentType ref 广播的响应类型。
	AdvertisementContentType = "application/x-git-upload-pack-advertisement"
	// RequestContentType 协商请求的请求类型。
	RequestContentType = "application/x-git-upload-pack-request"
	// ResultContentType 协商应答的响应类型。
	ResultContentType = "application/x-git-upload-pack-result"
)

// uploadPackService 服务名，前导行里那一句。
const uploadPackService = "git-upload-pack"

// ErrNotAnswerable 这次请求本地答不了，调用方应当降级为穿透。
//
// 它是**默认路径**上的一个答复，不是错误处理的兜底：带 shallow 或 filter 的请求
// 本来就该由上游来答，katch 只是把它原样转过去。分成一个可判定的错误而不是
// (nil, nil)，是因为「答不了」和「答了个空」在协议上完全是两回事。
var ErrNotAnswerable = errors.New("本地镜像答不了这个 git 请求")

// Advertise 生成一次 ref 广播的响应体。
//
// dir 是盘上那份裸仓库。返回的字节可以直接作为
// application/x-git-upload-pack-advertisement 发出去。
func Advertise(ctx context.Context, dir string) ([]byte, error) {
	session, err := uploadPackSession(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()

	refs, err := session.AdvertisedReferencesContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("读本地镜像的 ref 失败：%w", err)
	}
	// Prefix 就是为 smart HTTP 这一行准备的（packp.AdvRefs 的文档）：先一行
	// `# service=git-upload-pack`，再一个 flush，然后才是真正的广播。自己拼
	// pkt-line 只会多一处长度算错的地方。
	refs.Prefix = [][]byte{[]byte("# service=" + uploadPackService), pktline.Flush}
	// go-git 只广播 agent 与 ofs-delta，这里补上 shallow：客户端在发 --depth
	// 之前看的就是这一项，不广播它 `git clone --depth 1` 会当场 die 在
	// 「Server does not support shallow clients」上。这不是谎话——katch 作为
	// 一整台服务器确实答得了 shallow，只是那一次由穿透去答（能力边界一节）。
	// 它也不会污染协商：shallow 不在客户端回敬的能力列表里。
	if err := refs.Capabilities.Add(capability.Shallow); err != nil {
		return nil, fmt.Errorf("广播 shallow 能力失败：%w", err)
	}
	var buf bytes.Buffer
	if err := refs.Encode(&buf); err != nil {
		return nil, fmt.Errorf("编码 ref 广播失败：%w", err)
	}
	return buf.Bytes(), nil
}

// UploadPack 按客户端的协商请求生成一次应答的响应体。
//
// body 是原样的请求体（application/x-git-upload-pack-request）。返回的流可以
// 直接作为 application/x-git-upload-pack-result 发出去，调用方负责关闭它。
//
// 本地答不了时给回 ErrNotAnswerable，且**在写出任何字节之前**：一次应答一旦
// 开了头就没法再改主意穿透，所以对象图的遍历要在这里就跑完（go-git 的
// UploadPack 正是这么做的，返回之后才轮到打包）。
func UploadPack(ctx context.Context, dir string, body []byte) (io.ReadCloser, error) {
	request, err := decodeRequest(body)
	if err != nil {
		return nil, err
	}
	session, err := uploadPackSession(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()

	response, err := session.UploadPack(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("本地应答 upload-pack 失败：%w", err)
	}
	// 一边算一边发：pack 是现打的，先落进内存再发会让一个大仓库把内存吃光，
	// 也会把首字节推迟到整个 pack 编完。
	reader, writer := io.Pipe()
	go func() {
		// Encode 内部会把 response 手上那个 pack 流关掉。
		_ = writer.CloseWithError(response.Encode(writer))
	}()
	return reader, nil
}

// decodeRequest 把请求体解成 go-git 认得的协商请求。
//
// 请求体的形态是：want 行若干（第一行带能力集）、可能的 shallow/deepen/filter 行、
// 一个 flush，然后是 have 行与 done。go-git 只给了前半段的解码器（UploadRequest），
// have 那一段是客户端侧编码的，服务端这边得自己读。
func decodeRequest(body []byte) (*packp.UploadPackRequest, error) {
	head, rest, err := splitAtFlush(body)
	if err != nil {
		return nil, err
	}
	if err := rejectUnsupported(head); err != nil {
		return nil, err
	}
	request := packp.NewUploadPackRequest()
	if err := request.Decode(bytes.NewReader(head)); err != nil {
		// 解不开的请求不当错误往客户端抛：我们只本地应答完全看得懂的那些，
		// 其余原样交给上游——上游的 git 比这里的解码器认得多。
		return nil, fmt.Errorf("%w：请求解不开（%s）", ErrNotAnswerable, err)
	}
	haves, err := decodeHaves(rest)
	if err != nil {
		return nil, err
	}
	request.Haves = haves
	if request.IsEmpty() {
		return nil, fmt.Errorf("%w：请求里没有要的对象", ErrNotAnswerable)
	}
	return request, nil
}

// 请求体里出现这几种行，就说明客户端要的是本地给不了的东西。
var unsupportedLines = [][]byte{
	// shallow：客户端手上已经是一份浅仓库。
	[]byte("shallow "),
	// deepen / deepen-since / deepen-not：--depth 系列。
	[]byte("deepen"),
	// filter：partial clone。
	[]byte("filter "),
}

// rejectUnsupported 扫一遍请求的前半段，挑出本地答不了的形态。
//
// 判据放在解码**之前**：go-git 的解码器对 filter 行只会给一句「意料之外的行」，
// 而这两件事在日志里要分得开——一个是我们已知答不了，一个是我们没看懂。
func rejectUnsupported(head []byte) error {
	scanner := pktline.NewScanner(bytes.NewReader(head))
	for scanner.Scan() {
		line := scanner.Bytes()
		for _, prefix := range unsupportedLines {
			if bytes.HasPrefix(line, prefix) {
				return fmt.Errorf("%w：%s", ErrNotAnswerable,
					bytes.TrimSpace(line[:len(prefix)]))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w：请求的 pkt-line 读不下去（%s）", ErrNotAnswerable, err)
	}
	return nil
}

// decodeHaves 读 flush 之后那一段里的 have 行。
//
// 客户端在 clone 时一条 have 都没有，fetch 时则会带上它手上已有的提交——少读了
// 它们只是把一份客户端已经有的对象再发一遍，读错了才会发出一个缺对象的 pack。
func decodeHaves(rest []byte) ([]plumbing.Hash, error) {
	var haves []plumbing.Hash
	scanner := pktline.NewScanner(bytes.NewReader(rest))
	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte("\n"))
		suffix, ok := bytes.CutPrefix(line, []byte("have "))
		if !ok {
			continue
		}
		hash := plumbing.NewHash(string(suffix))
		if hash.IsZero() {
			return nil, fmt.Errorf("%w：have 行里不是一个 hash", ErrNotAnswerable)
		}
		haves = append(haves, hash)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w：have 段读不下去（%s）", ErrNotAnswerable, err)
	}
	return haves, nil
}

// splitAtFlush 在第一个 flush-pkt 处把请求体切成两段，flush 本身留在前一段。
//
// 自己走一遍长度前缀而不是用 pktline.Scanner：Scanner 会从底下的 reader 多读，
// 切完之后剩下的那一段就不完整了，而 have 段正落在那里。
func splitAtFlush(body []byte) ([]byte, []byte, error) {
	for offset := 0; offset+4 <= len(body); {
		length, err := pktLineLength(body[offset : offset+4])
		if err != nil {
			return nil, nil, err
		}
		if length == 0 {
			return body[:offset+4], body[offset+4:], nil
		}
		if length < 4 || offset+length > len(body) {
			return nil, nil, fmt.Errorf("%w：pkt-line 长度越界", ErrNotAnswerable)
		}
		offset += length
	}
	return nil, nil, fmt.Errorf("%w：请求里没有 flush-pkt", ErrNotAnswerable)
}

// pktLineLength 解一行的四位十六进制长度前缀。
func pktLineLength(prefix []byte) (int, error) {
	length := 0
	for _, c := range prefix {
		digit, err := hexDigit(c)
		if err != nil {
			return 0, err
		}
		length = length<<4 | digit
	}
	return length, nil
}

func hexDigit(c byte) (int, error) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), nil
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, nil
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, nil
	}
	return 0, fmt.Errorf("%w：pkt-line 长度前缀不是十六进制", ErrNotAnswerable)
}

// uploadPackSession 在一份盘上的裸仓库上开一次只读会话。
//
// 走 MapLoader 而不是 FilesystemLoader：后者要把目录路径塞进 endpoint 再解析回来，
// 而镜像目录来自上游的仓库路径，多一次「路径 → URL → 路径」的往返就多一处能
// 跑出根目录的地方。这里直接把仓库交给它，endpoint 只是个查表用的名字。
func uploadPackSession(dir string) (transport.UploadPackSession, error) {
	// config 不在就说明这里没有一份裸仓库：库里写着 ready 而目录被人删了、
	// 或者上一次建镜像只写了一半。两种都不是错误，是「这次穿透」。
	if _, err := os.Stat(filepath.Join(dir, "config")); err != nil {
		return nil, fmt.Errorf("%w：盘上没有这份镜像", ErrNotAnswerable)
	}
	endpoint, err := transport.NewEndpoint("/")
	if err != nil {
		return nil, fmt.Errorf("构造本地端点失败：%w", err)
	}
	storage := filesystem.NewStorage(osfs.New(dir), cache.NewObjectLRUDefault())
	session, err := server.NewServer(server.MapLoader{endpoint.String(): storage}).
		NewUploadPackSession(endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("打开本地镜像失败：%w", err)
	}
	return session, nil
}
