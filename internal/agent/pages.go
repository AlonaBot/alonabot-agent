package agent

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AlonaBot/AlonaBot/sdk"
	conversationapi "github.com/AlonaBot/AlonaBot/sdk/conversation"
	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/AlonaBot/sdk/gormdb"
	toolapi "github.com/AlonaBot/AlonaBot/sdk/tool"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

//go:embed pages
var pageFiles embed.FS

// conversationRecord 只记录本插件写过的完整会话键，正文仍由 Core 保存。
type conversationRecord struct {
	Key string `gorm:"primaryKey;autoIncrement:false" json:"key"`
}

// TableName 固定会话目录的受管表名。
func (conversationRecord) TableName() string { return "conversation_record" }

func (a *component) registerPages(p *sdk.Plugin) {
	for _, declaration := range []struct{ path, title string }{
		{"personas", "Personas"}, {"mcp", "MCP"}, {"skills", "Skills"}, {"memory", "Memory"}, {"tasks", "User tasks"},
	} {
		p.Route(http.MethodGet, "/pages/"+declaration.path).Page(declaration.title).HandleFunc(a.servePage)
	}
	assets, err := fs.Sub(pageFiles, "pages/assets")
	if err != nil {
		panic(err)
	} // embed 的目录在构建时确定。
	p.Route(http.MethodGet, "/pages/assets/").Public(sdk.AuthStatic).Handle(http.StripPrefix("/pages/assets/", http.FileServerFS(assets)))
	for _, route := range []struct{ method, path string }{
		{"GET", "personas"}, {"POST", "personas"}, {"PUT", "personas"}, {"DELETE", "personas"},
		{"GET", "mcp"}, {"POST", "mcp/reconnect"}, {"GET", "skills"}, {"POST", "skills/reload"},
		{"GET", "memory"}, {"PATCH", "memory"}, {"DELETE", "memory"},
		{"GET", "conversation"}, {"GET", "tasks"}, {"DELETE", "tasks"},
	} {
		p.Route(route.method, "/pages/api/"+route.path).HandleFunc(a.pageData)
	}
}

func (a *component) servePage(w http.ResponseWriter, r *http.Request) {
	body, err := pageFiles.ReadFile("pages/index.html")
	if err != nil {
		pageError(w, r, fmt.Errorf("read embedded page: %w", err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(body); err != nil {
		slog.DebugContext(r.Context(), "write agent page failed", "error", err)
	}
}

type pageFailure struct {
	Description string             `json:"description"`
	Code        string             `json:"code,omitempty"`
	References  []personaReference `json:"references,omitempty"`
}

func pageError(w http.ResponseWriter, r *http.Request, err error) {
	code := errcode.Of(err)
	status, description := http.StatusInternalServerError, "Could not complete the operation."
	switch code {
	case errcode.InvalidArgument:
		status, description = http.StatusBadRequest, err.Error()
	case errcode.NotFound:
		status, description = http.StatusNotFound, "The requested record does not exist."
	case errcode.AlreadyExists:
		status, description = http.StatusConflict, "A persona with this name already exists."
	case errcode.ResourceExhausted:
		status, description = http.StatusBadRequest, "Memory exceeds the Unicode code point limit."
	}
	slog.ErrorContext(r.Context(), "agent page operation failed", "path", r.URL.Path, "error", err)
	failure := pageFailure{Description: description}
	if code != 0 {
		failure.Code = code.String()
	}
	writeModelJSON(w, status, failure)
}

func decodePageRequest(r *http.Request, value any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode page request: %w", errcode.InvalidArgument.New(err.Error()))
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return errcode.InvalidArgument.New("request must contain one JSON object")
	}
	return nil
}

func (a *component) pageData(w http.ResponseWriter, r *http.Request) {
	var result any
	var err error
	status := http.StatusOK
	switch strings.TrimPrefix(r.URL.Path, "/pages/api/") {
	case "personas":
		result, status, err = a.pagePersonas(r)
	case "mcp":
		result, err = a.pageMCP(r, "")
	case "mcp/reconnect":
		var input struct {
			Name string `json:"name"`
		}
		err = decodePageRequest(r, &input)
		if err == nil {
			if input.Name == "" {
				err = errcode.InvalidArgument.New("name is required")
			} else {
				result, err = a.pageMCP(r, input.Name)
			}
		}
	case "skills", "skills/reload":
		if r.Method == http.MethodPost {
			if r.ContentLength != 0 {
				if err = decodePageRequest(r, &struct{}{}); err != nil {
					break
				}
			}
			a.reloadSkills()
		}
		skills, problems := a.skillSnapshot()
		listed := []map[string]string{}
		for _, skill := range skills {
			listed = append(listed, map[string]string{"name": skill.name, "description": skill.description})
		}
		result = struct {
			Skills   []map[string]string `json:"skills"`
			Problems []skillProblem      `json:"problems"`
		}{listed, problems}
	case "memory":
		result, err = a.pageMemory(r)
	case "conversation":
		result, err = a.pageConversation(r)
	case "tasks":
		result, err = a.pageTasks(r)
	}
	if err != nil {
		pageError(w, r, err)
		return
	}
	writeModelJSON(w, status, result)
}

func (a *component) pagePersonas(r *http.Request) (any, int, error) {
	db, err := gormdb.Open(r.Context())
	if err != nil {
		return nil, 0, err
	}
	db = db.WithContext(r.Context())
	if r.Method == http.MethodGet {
		rows := []persona{}
		if err := db.Order("name").Find(&rows).Error; err != nil {
			return nil, 0, err
		}
		references, err := a.personaReferences(r.Context())
		if err != nil {
			return nil, 0, err
		}
		type listedPersona struct {
			persona
			References []personaReference `json:"references"`
		}
		listed := []listedPersona{}
		for _, row := range rows {
			refs := references[row.Name]
			if refs == nil {
				refs = []personaReference{}
			}
			listed = append(listed, listedPersona{row, refs})
		}
		return struct {
			Personas []listedPersona `json:"personas"`
		}{listed}, http.StatusOK, nil
	}
	var request struct {
		Name   string  `json:"name"`
		Prompt *string `json:"prompt"`
	}
	if r.Method == http.MethodDelete {
		request.Name = r.URL.Query().Get("name")
	} else if err := decodePageRequest(r, &request); err != nil {
		return nil, 0, err
	}
	if strings.TrimSpace(request.Name) == "" {
		return nil, 0, errcode.InvalidArgument.New("name is required")
	}
	if r.Method != http.MethodDelete && request.Prompt == nil {
		return nil, 0, errcode.InvalidArgument.New("prompt is required")
	}
	input := persona{Name: request.Name}
	if request.Prompt != nil {
		input.Prompt = *request.Prompt
	}
	if r.Method == http.MethodPost {
		created := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&input)
		if created.Error != nil {
			return nil, 0, created.Error
		}
		if created.RowsAffected == 0 {
			return nil, 0, errcode.AlreadyExists.New("duplicate persona name")
		}
		return input, http.StatusCreated, nil
	}
	if _, err := a.readPersona(r.Context(), input.Name); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			err = errcode.NotFound.New("unknown persona")
		}
		return nil, 0, err
	}
	if r.Method == http.MethodDelete {
		references, err := a.personaReferences(r.Context())
		if err != nil {
			return nil, 0, err
		}
		if len(references[input.Name]) > 0 {
			return pageFailure{Description: "This persona is referenced by configuration profiles.", References: references[input.Name]}, http.StatusConflict, nil
		}
		return struct{}{}, http.StatusOK, db.Delete(&input).Error
	}
	updated := db.Model(&persona{}).Where("name = ?", input.Name).Update("prompt", input.Prompt)
	if updated.Error != nil {
		return nil, 0, updated.Error
	}
	if updated.RowsAffected == 0 {
		return nil, 0, errcode.NotFound.New("unknown persona")
	}
	return input, http.StatusOK, nil
}

type pageMCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Effect      toolapi.Effect `json:"effect"`
}
type pageMCPServer struct {
	Name      string        `json:"name"`
	Transport string        `json:"transport"`
	Status    string        `json:"status"`
	Tools     []pageMCPTool `json:"tools"`
	Error     string        `json:"error,omitempty"`
}

func (p *mcpPeer) pageStatus(r *http.Request) (pageMCPServer, error) {
	select {
	case p.gate <- struct{}{}:
	case <-r.Context().Done():
		return pageMCPServer{}, r.Context().Err()
	}
	defer func() { <-p.gate }()
	result := pageMCPServer{Name: p.config.Name, Transport: p.config.Transport, Status: "disconnected", Tools: []pageMCPTool{}}
	if p.session != nil {
		result.Status = "connected"
		// gate 内只接收当前会话的结束通知，保留最近成功的工具目录。
		if ended := p.sessionEnd; ended.session == p.session {
			select {
			case <-ended.done:
				p.lastError = ended.err
				if p.lastError == nil {
					p.lastError = fmt.Errorf("MCP session closed")
				}
			default:
			}
		}
	}
	if p.lastError != nil {
		result.Status = "error"
		result.Error = p.config.sanitize(p.lastError.Error())
	}
	for _, tool := range p.lastTools {
		result.Tools = append(result.Tools, pageMCPTool{Name: p.config.sanitize(tool.Name), Description: p.config.sanitize(tool.Description), Effect: mcpEffect(tool.Annotations)})
	}
	return result, nil
}

func (a *component) pageMCP(r *http.Request, name string) (any, error) {
	servers := []pageMCPServer{}
	for _, peer := range a.mcp {
		if name != "" && peer.config.Name != name {
			continue
		}
		if name != "" {
			if err := peer.reconnect(r.Context()); err != nil {
				slog.WarnContext(r.Context(), "MCP reconnect failed", "server", peer.config.sanitize(name), "error", peer.config.sanitize(err.Error()))
			}
		}
		server, err := peer.pageStatus(r)
		if err != nil {
			return nil, err
		}
		if name != "" {
			return server, nil
		}
		servers = append(servers, server)
	}
	if name != "" {
		return nil, errcode.NotFound.New("unknown MCP server")
	}
	return struct {
		Servers []pageMCPServer `json:"servers"`
	}{servers}, nil
}

func (a *component) pageMemory(r *http.Request) (any, error) {
	db, err := gormdb.Open(r.Context())
	if err != nil {
		return nil, err
	}
	db = db.WithContext(r.Context())
	if r.Method == http.MethodGet {
		records := []memoryRecord{}
		if err := db.Order("target, scope, instance").Find(&records).Error; err != nil {
			return nil, err
		}
		for i := range records {
			if records[i].Entries == nil {
				records[i].Entries = []string{}
			}
		}
		return struct {
			Records []memoryRecord `json:"records"`
		}{records}, nil
	}
	var input struct {
		Target     string `json:"target"`
		Scope      string `json:"scope"`
		Instance   string `json:"instance"`
		Operations []struct {
			Op      string `json:"op"`
			Text    string `json:"text,omitempty"`
			OldText string `json:"old_text,omitempty"`
		} `json:"operations"`
	}
	if err := decodePageRequest(r, &input); err != nil {
		return nil, err
	}
	if (input.Target != memoryTarget && input.Target != userTarget) || input.Scope == "" || input.Instance == "" {
		return nil, errcode.InvalidArgument.New("target, scope and instance are required")
	}
	scope := memoryRecord{Target: input.Target, Scope: input.Scope, Instance: input.Instance}
	if r.Method == http.MethodDelete {
		return struct{}{}, db.Delete(&scope).Error
	}
	if len(input.Operations) == 0 {
		return nil, errcode.InvalidArgument.New("operations must not be empty")
	}
	operations := make([]memoryOperation, len(input.Operations))
	for i, operation := range input.Operations {
		operations[i] = memoryOperation{Target: input.Target, Op: operation.Op, Text: operation.Text, OldText: operation.OldText}
	}
	if err := updateMemory(db, map[string]memoryRecord{input.Target: scope}, operations); err != nil {
		return nil, err
	}
	record, err := readMemory(db, scope)
	if record.Entries == nil {
		record.Entries = []string{}
	}
	return record, err
}

func (a *component) pageConversation(r *http.Request) (any, error) {
	query := r.URL.Query()
	key := query.Get("key")
	if key == "" {
		db, err := gormdb.Open(r.Context())
		if err != nil {
			return nil, err
		}
		records := []conversationRecord{}
		if err := db.WithContext(r.Context()).Order("key").Find(&records).Error; err != nil {
			return nil, err
		}
		existing := make([]conversationRecord, 0, len(records))
		for _, record := range records {
			exists, err := func() (bool, error) {
				// 与会话追加和 reset 串行，空目录清理不会删掉刚登记的新会话。
				lock := a.sessionLock(record.Key)
				lock.Lock()
				defer lock.Unlock()
				entries, _, _, err := conversationapi.List(r.Context(), record.Key, 0)
				if err != nil {
					return false, fmt.Errorf("check conversation directory: %w", err)
				}
				if len(entries) == 0 {
					return false, db.WithContext(r.Context()).Delete(&record).Error
				}
				return true, nil
			}()
			if err != nil {
				return nil, err
			}
			if exists {
				existing = append(existing, record)
			}
		}

		records = existing
		return struct {
			Conversations []conversationRecord `json:"conversations"`
		}{records}, err
	}
	var from uint64
	if raw := query.Get("from_seq"); raw != "" {
		var err error
		from, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return nil, errcode.InvalidArgument.New("from_seq must be an unsigned integer")
		}
	}
	entries, next, more, err := conversationapi.List(r.Context(), key, from)
	if err != nil {
		return nil, fmt.Errorf("list conversation: %w", err)
	}
	type entry struct {
		Seq   uint64          `json:"seq"`
		Entry json.RawMessage `json:"entry"`
	}
	listed := make([]entry, 0, len(entries))
	for _, stored := range entries {
		listed = append(listed, entry{stored.Seq, stored.Entry})
	}
	return struct {
		Entries []entry `json:"entries"`
		NextSeq uint64  `json:"next_seq"`
		More    bool    `json:"more"`
	}{listed, next, more}, nil
}

func (a *component) pageTasks(r *http.Request) (any, error) {
	if r.Method == http.MethodDelete {
		key := r.URL.Query().Get("key")
		if key == "" {
			return nil, errcode.InvalidArgument.New("key is required")
		}
		return struct{}{}, agentTask.Cancel(r.Context(), key)
	}
	instances, err := agentTask.List(r.Context())
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	type listedTask struct {
		Key string `json:"key"`
		scheduledPrompt
		At       *time.Time `json:"at,omitempty"`
		Interval int64      `json:"interval_seconds,omitempty"`
		Cron     string     `json:"cron,omitempty"`
		Timezone string     `json:"timezone,omitempty"`
		NextAt   *time.Time `json:"next_at"`
	}
	tasks := []listedTask{}
	for _, instance := range instances {
		task := listedTask{Key: instance.Key, scheduledPrompt: instance.Args, Interval: int64(instance.Interval / time.Second), Cron: instance.Cron, Timezone: instance.Timezone, NextAt: instance.NextAt}
		if !instance.At.IsZero() {
			task.At = &instance.At
		}
		tasks = append(tasks, task)
	}
	return struct {
		Tasks []listedTask `json:"tasks"`
	}{tasks}, nil
}
