package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/AlonaBot/sdk/gormdb"
	"gorm.io/gorm"
)

const mcpCloseTimeout = 5 * time.Second

// managedMCPServer 保存经 Agent 页面创建与编辑的 MCP 服务器配置。
type managedMCPServer struct {
	Name       string `gorm:"primaryKey;autoIncrement:false"`
	ConfigJSON string `gorm:"not null"`
}

// TableName 固定受管数据库中 MCP 服务器配置表名。
func (managedMCPServer) TableName() string { return "managed_mcp_server" }

type mcpServerInput struct {
	Name      string       `json:"name"`
	Transport string       `json:"transport"`
	Command   string       `json:"command,omitempty"`
	Args      []string     `json:"args,omitempty"`
	URL       string       `json:"url,omitempty"`
	Env       []secretPair `json:"env,omitempty"`
	Headers   []secretPair `json:"headers,omitempty"`
	KeepEnv   []string     `json:"keep_env,omitempty"`
	KeepHdrs  []string     `json:"keep_headers,omitempty"`
}

// validateMCPServer 校验单个 MCP 服务器配置，静态配置与页面托管配置共用同一规则。
func validateMCPServer(server mcpServer) error {
	if strings.TrimSpace(server.Name) == "" {
		return errcode.InvalidArgument.New("MCP server name must be nonempty")
	}
	switch server.Transport {
	case mcpStdio:
		if strings.TrimSpace(server.Command) == "" {
			return errcode.InvalidArgument.New("MCP stdio command must be nonempty")
		}
		for _, pair := range server.Env {
			if pair.Name == "" || strings.ContainsAny(pair.Name, "=\x00") || strings.ContainsRune(pair.Value, 0) {
				return errcode.InvalidArgument.New("invalid MCP environment entry")
			}
		}
	case mcpHTTP:
		endpoint, err := url.Parse(server.URL)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.Fragment != "" {
			return errcode.InvalidArgument.New("MCP URL must be an HTTP(S) endpoint without a fragment")
		}
		for _, pair := range server.Headers {
			if !validHeaderName(pair.Name) || strings.ContainsAny(pair.Value, "\r\n\x00") {
				return errcode.InvalidArgument.New("invalid MCP header entry")
			}
		}
	default:
		return errcode.InvalidArgument.New("unsupported MCP transport")
	}
	return nil
}

// mcpSnapshot 返回当前 MCP peer 列表的浅拷贝，避免热增删与并发对话轮次发生数据竞争。
func (a *component) mcpSnapshot() []*mcpPeer {
	a.mcpMu.RLock()
	defer a.mcpMu.RUnlock()
	peers := make([]*mcpPeer, len(a.mcp))
	copy(peers, a.mcp)
	return peers
}

// loadManagedMCP 从受管 SQLite 读取页面托管的 MCP 服务器，忽略与静态配置重名的条目。
func (a *component) loadManagedMCP(ctx context.Context) error {
	db, err := gormdb.Open(ctx)
	if err != nil {
		return err
	}
	var rows []managedMCPServer
	if err := db.WithContext(ctx).Order("name").Find(&rows).Error; err != nil {
		return err
	}
	a.mcpMu.Lock()
	defer a.mcpMu.Unlock()
	existing := make(map[string]bool, len(a.mcp))
	for _, peer := range a.mcp {
		existing[peer.config.Name] = true
	}
	for _, row := range rows {
		var server mcpServer
		if err := json.Unmarshal([]byte(row.ConfigJSON), &server); err != nil {
			slog.WarnContext(ctx, "skip corrupted managed MCP server", "name", row.Name, "error", err)
			continue
		}
		server.Name = row.Name
		if existing[server.Name] {
			continue
		}
		if err := validateMCPServer(server); err != nil {
			slog.WarnContext(ctx, "skip invalid managed MCP server", "name", row.Name, "error", err)
			continue
		}
		peer := newMCPPeer(server)
		peer.managed = true
		a.mcp = append(a.mcp, peer)
		existing[server.Name] = true
	}
	return nil
}

func (a *component) findMCPPeer(name string) *mcpPeer {
	a.mcpMu.RLock()
	defer a.mcpMu.RUnlock()
	for _, peer := range a.mcp {
		if peer.config.Name == name {
			return peer
		}
	}
	return nil
}

func buildServerFromInput(input mcpServerInput, previous *mcpServer) (mcpServer, error) {
	server := mcpServer{
		Name:      strings.TrimSpace(input.Name),
		Transport: strings.TrimSpace(input.Transport),
		Command:   strings.TrimSpace(input.Command),
		URL:       strings.TrimSpace(input.URL),
	}
	for _, arg := range input.Args {
		server.Args = append(server.Args, arg)
	}
	// 1. 编辑时若未提交某秘密键的新值但保留在 keep 列表中，则沿用旧值。
	prevEnv := map[string]string{}
	prevHdrs := map[string]string{}
	if previous != nil {
		for _, pair := range previous.Env {
			prevEnv[pair.Name] = pair.Value
		}
		for _, pair := range previous.Headers {
			prevHdrs[pair.Name] = pair.Value
		}
	}
	seenEnv := map[string]bool{}
	for _, pair := range input.Env {
		key := strings.TrimSpace(pair.Name)
		if key == "" {
			continue
		}
		seenEnv[key] = true
		server.Env = append(server.Env, secretPair{Name: key, Value: pair.Value})
	}
	for _, key := range input.KeepEnv {
		if !seenEnv[key] {
			if value, ok := prevEnv[key]; ok {
				seenEnv[key] = true
				server.Env = append(server.Env, secretPair{Name: key, Value: value})
			}
		}
	}
	seenHdrs := map[string]bool{}
	for _, pair := range input.Headers {
		key := strings.TrimSpace(pair.Name)
		if key == "" {
			continue
		}
		seenHdrs[key] = true
		server.Headers = append(server.Headers, secretPair{Name: key, Value: pair.Value})
	}
	for _, key := range input.KeepHdrs {
		if !seenHdrs[key] {
			if value, ok := prevHdrs[key]; ok {
				seenHdrs[key] = true
				server.Headers = append(server.Headers, secretPair{Name: key, Value: value})
			}
		}
	}
	if err := validateMCPServer(server); err != nil {
		return mcpServer{}, err
	}
	return server, nil
}

func (a *component) pageMCPManage(r *http.Request) (any, int, error) {
	if r.Method == http.MethodGet {
		res, err := a.pageMCP(r, "")
		return res, http.StatusOK, err
	}
	db, err := gormdb.Open(r.Context())
	if err != nil {
		return nil, 0, err
	}
	db = db.WithContext(r.Context())

	switch r.Method {
	case http.MethodDelete:
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			return nil, 0, errcode.InvalidArgument.New("name is required")
		}
		peer := a.findMCPPeer(name)
		if peer == nil {
			return nil, 0, errcode.NotFound.New("unknown MCP server")
		}
		if !peer.managed {
			return nil, 0, errcode.InvalidArgument.New("static config MCP server cannot be deleted here")
		}
		if err := db.Delete(&managedMCPServer{Name: name}).Error; err != nil {
			return nil, 0, err
		}
		a.mcpMu.Lock()
		for i, item := range a.mcp {
			if item == peer {
				a.mcp = append(a.mcp[:i], a.mcp[i+1:]...)
				break
			}
		}
		a.mcpMu.Unlock()
		closeCtx, cancel := context.WithTimeout(context.Background(), mcpCloseTimeout)
		defer cancel()
		if closeErr := peer.close(closeCtx); closeErr != nil {
			slog.WarnContext(r.Context(), "close removed MCP server failed", "server", peer.config.sanitize(name), "error", peer.config.sanitize(closeErr.Error()))
		}
		return struct{}{}, http.StatusOK, nil

	case http.MethodPost:
		// 支持单条创建或批量导入（剪贴板识别多台服务器）。
		var raw struct {
			Servers []mcpServerInput `json:"servers"`
			mcpServerInput
		}
		if err := decodePageRequest(r, &raw); err != nil {
			return nil, 0, err
		}
		inputs := raw.Servers
		if len(inputs) == 0 {
			inputs = []mcpServerInput{raw.mcpServerInput}
		}
		parsed := make([]mcpServer, 0, len(inputs))
		batchNames := make(map[string]bool, len(inputs))
		for _, item := range inputs {
			server, err := buildServerFromInput(item, nil)
			if err != nil {
				return nil, 0, err
			}
			if batchNames[server.Name] || a.findMCPPeer(server.Name) != nil {
				return nil, 0, errcode.AlreadyExists.New("duplicate MCP server name: " + server.Name)
			}
			batchNames[server.Name] = true
			parsed = append(parsed, server)
		}
		if err := db.Transaction(func(tx *gorm.DB) error {
			for _, server := range parsed {
				encoded, err := json.Marshal(server)
				if err != nil {
					return err
				}
				if err := tx.Create(&managedMCPServer{Name: server.Name, ConfigJSON: string(encoded)}).Error; err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return nil, 0, err
		}
		a.mcpMu.Lock()
		for _, server := range parsed {
			peer := newMCPPeer(server)
			peer.managed = true
			a.mcp = append(a.mcp, peer)
		}
		a.mcpMu.Unlock()
		res, err := a.pageMCP(r, "")
		return res, http.StatusCreated, err

	case http.MethodPut:
		var input mcpServerInput
		if err := decodePageRequest(r, &input); err != nil {
			return nil, 0, err
		}
		existing := a.findMCPPeer(strings.TrimSpace(input.Name))
		if existing == nil {
			return nil, 0, errcode.NotFound.New("unknown MCP server")
		}
		if !existing.managed {
			return nil, 0, errcode.InvalidArgument.New("static config MCP server cannot be edited here")
		}
		server, err := buildServerFromInput(input, &existing.config)
		if err != nil {
			return nil, 0, err
		}
		encoded, err := json.Marshal(server)
		if err != nil {
			return nil, 0, err
		}
		if err := db.Save(&managedMCPServer{Name: server.Name, ConfigJSON: string(encoded)}).Error; err != nil {
			return nil, 0, err
		}
		replacement := newMCPPeer(server)
		replacement.managed = true
		a.mcpMu.Lock()
		for i, item := range a.mcp {
			if item == existing {
				a.mcp[i] = replacement
				break
			}
		}
		a.mcpMu.Unlock()
		closeCtx, cancel := context.WithTimeout(context.Background(), mcpCloseTimeout)
		defer cancel()
		if closeErr := existing.close(closeCtx); closeErr != nil && !errors.Is(closeErr, context.Canceled) {
			slog.WarnContext(r.Context(), "close replaced MCP server failed", "server", existing.config.sanitize(server.Name), "error", existing.config.sanitize(closeErr.Error()))
		}
		status, err := replacement.pageStatus(r)
		return status, http.StatusOK, err
	}
	return nil, 0, errcode.InvalidArgument.New("unsupported method")
}
