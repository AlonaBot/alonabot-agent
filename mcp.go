package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/AlonaBot/AlonaBot/sdk/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpStdio = "stdio"
	mcpHTTP  = "streamable-http"
	redacted = "[REDACTED]"
)

type secretPair struct{ Name, Value string }

type mcpServer struct {
	Name, Transport, Command, URL string
	Args                          []string
	Env, Headers                  []secretPair
}

func readMCPServers(values config.Values) ([]mcpServer, error) {
	var servers []mcpServer
	names := make(map[string]bool)
	for _, value := range values.Objects("mcp_servers") {
		server := mcpServer{Name: value.String("name"), Transport: value.String("transport")}
		if server.Name == "" || names[server.Name] {
			return nil, fmt.Errorf("MCP server names must be nonempty and unique")
		}
		names[server.Name] = true
		switch server.Transport {
		case mcpStdio:
			server.Command, server.Args = value.String("command"), value.Strings("args")
			if server.Command == "" {
				return nil, fmt.Errorf("MCP stdio command must be nonempty")
			}
			for _, pair := range value.Objects("env") {
				name, secret := pair.String("name"), pair.String("value")
				if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(secret, 0) {
					return nil, fmt.Errorf("invalid MCP environment entry")
				}
				server.Env = append(server.Env, secretPair{Name: name, Value: secret})
			}
		case mcpHTTP:
			server.URL = value.String("url")
			endpoint, err := url.Parse(server.URL)
			if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.Fragment != "" {
				return nil, fmt.Errorf("MCP URL must be an HTTP(S) endpoint without a fragment")
			}
			for _, pair := range value.Objects("headers") {
				name, secret := pair.String("name"), pair.String("value")
				if !validHeaderName(name) || strings.ContainsAny(secret, "\r\n\x00") {
					return nil, fmt.Errorf("invalid MCP header entry")
				}
				server.Headers = append(server.Headers, secretPair{Name: name, Value: secret})
			}
		default:
			return nil, fmt.Errorf("unsupported MCP transport")
		}
		servers = append(servers, server)
	}
	return servers, nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, ch := range name {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", ch)) {
			return false
		}
	}
	return true
}

// mcpPeer 的 gate 串行化首次连接和目录读取；工具执行使用本轮捕获的会话。
// stdio 通过 SDK 的 IOTransport 编解码，进程等待由此处负责，以使用实际停止预算。
type mcpPeer struct {
	config     mcpServer
	gate       chan struct{}
	session    *mcp.ClientSession // gate 保护；会话可并发 CallTool。
	command    *exec.Cmd
	stdin      io.WriteCloser // 停止预算到期时打断在途管道 I/O。
	stdout     io.ReadCloser
	exited     chan struct{} // 关闭发布 Wait 的结果，确保每个进程只 Wait 一次。
	waitError  error
	httpCancel context.CancelFunc
}

func newMCPPeer(server mcpServer) *mcpPeer {
	return &mcpPeer{config: server, gate: make(chan struct{}, 1)}
}

func (p *mcpPeer) tools(ctx context.Context) (*mcp.ClientSession, []*mcp.Tool, error) {
	select { // 等待另一轮的首次连接时仍服从本轮取消。
	case p.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	defer func() { <-p.gate }()
	if p.session == nil {
		if err := p.connect(ctx); err != nil {
			return nil, nil, err
		}
	}
	var tools []*mcp.Tool
	params := &mcp.ListToolsParams{}
	for {
		page, err := p.session.ListTools(ctx, params)
		if err != nil {
			return nil, nil, err
		}
		tools = append(tools, page.Tools...)
		if page.NextCursor == "" {
			break
		}
		params.Cursor = page.NextCursor
	}
	return p.session, tools, nil
}

func (p *mcpPeer) connect(ctx context.Context) error {
	var transport mcp.Transport
	switch p.config.Transport {
	case mcpStdio:
		command := exec.Command(p.config.Command, p.config.Args...)
		// 只传本服务器的变量；不继承 Core 管理凭证、插件认证与其他服务器的秘密。
		command.Env = []string{}
		for _, pair := range p.config.Env {
			command.Env = append(command.Env, pair.Name+"="+pair.Value)
		}
		stdout, err := command.StdoutPipe()
		if err != nil {
			return err
		}
		stdin, err := command.StdinPipe()
		if err != nil {
			return errors.Join(err, stdout.Close())
		}
		if err := command.Start(); err != nil {
			return errors.Join(err, stdout.Close(), stdin.Close())
		}
		p.command, p.exited = command, make(chan struct{})
		p.stdin, p.stdout = stdin, stdout
		go func() { p.waitError = command.Wait(); close(p.exited) }() // 通道关闭后才读 waitError。
		transport = &mcp.IOTransport{Reader: stdout, Writer: stdin}
	case mcpHTTP:
		lifetime, cancel := context.WithCancel(context.Background())
		p.httpCancel = cancel
		// 初始化失败时 SDK 同步 Close；它的 DELETE 也须随初始化调用取消。
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		transport = &mcp.StreamableClientTransport{Endpoint: p.config.URL,
			HTTPClient: &http.Client{Transport: &mcpHeaders{headers: p.config.Headers, lifetime: lifetime}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
			MaxRetries: -1, DisableStandaloneSSE: true}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "alonabot-agent", Version: "0.1.0"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		// 连接初始化失败时立即回收子进程；下一轮可重新建立连接。
		if p.command != nil {
			err = errors.Join(err, p.killAndWait())
		}
		if p.httpCancel != nil {
			p.httpCancel()
			p.httpCancel = nil
		}
		return err
	}
	p.session = session
	return nil
}

func (p *mcpPeer) killAndWait() error {
	var err error
	if killError := p.command.Process.Kill(); killError != nil && !errors.Is(killError, os.ErrProcessDone) {
		err = killError
	}
	<-p.exited
	p.command = nil
	return errors.Join(err, p.waitError)
}

func (p *mcpPeer) close(ctx context.Context) error {
	p.gate <- struct{}{}
	defer func() { <-p.gate }()
	var errs []error
	if p.httpCancel != nil {
		// SDK Close 的 DELETE 使用会话 context；将剩余停止预算传到实际 HTTP 请求。
		stop := context.AfterFunc(ctx, p.httpCancel)
		defer stop()
		defer p.httpCancel()
	}
	if p.session != nil {
		closed := make(chan error, 1)
		go func() { closed <- p.session.Close() }()
		select { // Close 等待在途写入，停止预算必须覆盖这段等待。
		case err := <-closed:
			errs = append(errs, err)
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
			if p.command != nil {
				errs = append(errs, p.stdin.Close(), p.stdout.Close(), p.killAndWait())
			}
			errs = append(errs, <-closed) // 管道中断后等待会话回收，再释放 gate。
		}
		p.session = nil
	}
	if p.command != nil {
		select { // stdin 关闭后等正常退出；预算耗尽时终止并等待，避免遗留僵尸进程。
		case <-p.exited:
			errs = append(errs, p.waitError)
			p.command = nil
		case <-ctx.Done():
			errs = append(errs, ctx.Err(), p.killAndWait())
		}
	}
	return errors.Join(errs...)
}

func (a *component) closeMCP(ctx context.Context) error {
	var wg sync.WaitGroup
	errs := make([]error, len(a.mcp))
	for i, p := range a.mcp {
		wg.Go(func() {
			if err := p.close(ctx); err != nil {
				errs[i] = fmt.Errorf("close MCP server: %w", &mcpCloseError{cause: err, safe: p.config.sanitize(err.Error())})
			}
		})
	}
	wg.Wait() // 所有连接共享同一个绝对截止时间。
	return errors.Join(errs...)
}

// mcpCloseError 保留错误链，同时只让脱敏后的说明进入 SDK 日志。
type mcpCloseError struct {
	cause error
	safe  string
}

func (e *mcpCloseError) Error() string { return e.safe }
func (e *mcpCloseError) Unwrap() error { return e.cause }

type mcpHeaders struct {
	headers  []secretPair
	lifetime context.Context
}

func (t *mcpHeaders) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(request.Context())
	stop := context.AfterFunc(t.lifetime, cancel)
	request = request.Clone(ctx)
	request.GetBody = nil // 即使配置了 Idempotency-Key，net/http 也不得重放工具请求体。
	for _, header := range t.headers {
		request.Header.Set(header.Name, header.Value)
	}
	response, err := http.DefaultTransport.RoundTrip(request)
	// 响应体仍在读取时保留 context；调用方 Close 后释放关联。
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	if request.Method == http.MethodDelete {
		// SDK 不检查 DELETE 状态或关闭响应体，此处保留两类失败。
		err := response.Body.Close()
		stop()
		cancel()
		response.Body = http.NoBody
		if (response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices) && response.StatusCode != http.StatusMethodNotAllowed {
			err = errors.Join(fmt.Errorf("terminate MCP session: HTTP %d %s", response.StatusCode, http.StatusText(response.StatusCode)), err)
		}
		if err != nil {
			return nil, err
		}
		return response, nil
	}
	response.Body = &mcpResponseBody{ReadCloser: response.Body, stop: stop, cancel: cancel}
	return response, nil
}

type mcpResponseBody struct {
	io.ReadCloser
	stop   func() bool
	cancel context.CancelFunc
}

func (b *mcpResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.stop()
	b.cancel()
	return err
}

func (s mcpServer) sanitize(text string) string {
	var secrets []string
	for _, pair := range append(append([]secretPair(nil), s.Env...), s.Headers...) {
		secrets = append(secrets, pair.Value)
	}
	for _, header := range s.Headers {
		if strings.EqualFold(header.Name, "Authorization") || strings.EqualFold(header.Name, "Proxy-Authorization") {
			if _, credential, ok := strings.Cut(header.Value, " "); ok {
				secrets = append(secrets, strings.TrimSpace(credential))
			}
		}
	}
	if s.URL != "" {
		secrets = append(secrets, s.URL)
		if endpoint, err := url.Parse(s.URL); err == nil {
			secrets = append(secrets, endpoint.Redacted())
			if endpoint.User != nil {
				username := endpoint.User.Username()
				password, _ := endpoint.User.Password()
				secrets = append(secrets, username, password, base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
			}
			for _, values := range endpoint.Query() {
				for _, value := range values {
					secrets = append(secrets, value, url.QueryEscape(value))
				}
			}
			// 保留配置实际采用的编码形式，例如 %20 或小写百分号转义。
			for _, pair := range strings.Split(endpoint.RawQuery, "&") {
				if _, value, ok := strings.Cut(pair, "="); ok {
					secrets = append(secrets, value)
				}
			}
		}
	}
	var forms []string
	for _, secret := range secrets {
		if secret != "" {
			encodedJSON, _ := json.Marshal(secret) // string 总能编码；匹配工具结果中的 HTML 转义形式。
			encoded := strconv.Quote(secret)
			forms = append(forms, secret, string(encodedJSON[1:len(encodedJSON)-1]), encoded[1:len(encoded)-1])
		}
	}
	// 先匹配完整 URL 和认证值，且替换后的占位符不再次参与匹配。
	slices.SortFunc(forms, func(a, b string) int { return len(b) - len(a) })
	var replacements []string
	for _, form := range forms {
		replacements = append(replacements, form, redacted)
	}
	return strings.NewReplacer(replacements...).Replace(text)
}
