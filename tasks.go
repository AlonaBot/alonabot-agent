package agent

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AlonaBot/AlonaBot/agent/provider"
	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/AlonaBot/sdk/message"
	taskapi "github.com/AlonaBot/AlonaBot/sdk/task"
	toolapi "github.com/AlonaBot/AlonaBot/sdk/tool"
)

const minimumTaskInterval = 60 * time.Second

const taskSummaryBytes = 1024 // core.tool.confirm 的 UTF-8 字节上限。

var agentTask = taskapi.Define[AgentTask]("agent")

type taskCreator struct {
	Instance string `json:"instance"`
	User     string `json:"user"`
}

type taskTarget struct {
	Instance  string           `json:"instance"`
	Chat      string           `json:"chat"` // 不含话题的会话地址。
	Thread    string           `json:"thread"`
	TextLimit uint64           `json:"text_limit"` // 创建时的文本上限快照；0 表示无已知上限。
	TextUnit  message.TextUnit `json:"text_unit"`  // TextLimit 的计量单位。
}

// AgentTask 保存创建者、显式发送目标和后台一轮的提示词。
type AgentTask struct {
	Creator   taskCreator `json:"creator"`
	Target    taskTarget  `json:"target"`
	Prompt    string      `json:"prompt"`
	CreatedAt string      `json:"created_at"` // RFC 3339 纳秒时间。
}

func currentTaskTarget(c *sdk.Context) taskTarget {
	adapter, chat, thread, _ := message.ParseSID(c.Session())
	return taskTarget{Instance: c.Instance(), Chat: message.SID(adapter, chat, ""), Thread: thread}
}

func (t taskTarget) session() string {
	adapter, chat, _, _ := message.ParseSID(t.Chat)
	return message.SID(adapter, chat, t.Thread)
}

func (t taskTarget) sameChat(other taskTarget) bool {
	return t.Instance == other.Instance && t.Chat == other.Chat && t.Thread == other.Thread
}

func (a *component) addTaskTools(c *sdk.Context, catalog *toolCatalog) {
	for _, tool := range []roundTool{
		{local: func(args json.RawMessage) string { return scheduleTask(c, args) }, definition: provider.Tool{Name: "schedule_task", Description: "Schedule an agent prompt for this chat (Core Admin only). Set exactly one non-empty trigger; unused optional fields may be omitted.", InputSchema: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"},"at":{"type":"string","description":"RFC 3339 time; omit or use an empty string when unused."},"interval_seconds":{"type":"integer","description":"At least 60 seconds when used; omit or use 0 when unused.","anyOf":[{"const":0},{"minimum":60}]},"cron":{"type":"string","description":"Five-field cron expression; omit or use an empty string when unused."},"timezone":{"type":"string","description":"IANA timezone for cron only; omit or use an empty string for the default."}},"required":["prompt"],"additionalProperties":false}`)}},
		{local: func(args json.RawMessage) string { return listTasks(c) }, definition: provider.Tool{Name: "list_tasks", Description: "List scheduled agent tasks in this chat", InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}},
		{local: func(args json.RawMessage) string { return cancelTask(c, args) }, definition: provider.Tool{Name: "cancel_task", Description: "Cancel a scheduled agent task in this chat (Core Admin only)", InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`)}},
	} {
		catalog.byName[tool.definition.Name] = tool
		catalog.definitions = append(catalog.definitions, tool.definition)
	}
}

func scheduleTask(c *sdk.Context, arguments json.RawMessage) string {
	if !c.Has(sdk.Admin) {
		return "error: PERMISSION_DENIED"
	}
	// 1. 在调用 Core 前验证输入组合与 Agent 的间隔下限。
	var input struct {
		Prompt   string `json:"prompt"`
		At       string `json:"at"`
		Interval int64  `json:"interval_seconds"`
		Cron     string `json:"cron"`
		Timezone string `json:"timezone"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return toolResult("", errcode.InvalidArgument.New(err.Error()))
	}
	triggers := 0
	for _, present := range []bool{input.At != "", input.Interval != 0, input.Cron != ""} {
		if present {
			triggers++
		}
	}
	if strings.TrimSpace(input.Prompt) == "" || triggers != 1 || input.Timezone != "" && input.Cron == "" {
		return toolResult("", errcode.InvalidArgument.New("prompt and exactly one trigger are required; timezone is only valid with cron"))
	}
	var at time.Time
	var interval time.Duration
	var err error
	if input.At != "" {
		at, err = time.Parse(time.RFC3339, input.At)
	}
	if input.Interval != 0 {
		interval, err = time.ParseDuration(strconv.FormatInt(input.Interval, 10) + "s")
		if err == nil && interval < minimumTaskInterval {
			err = errcode.InvalidArgument.New("interval_seconds must be at least 60")
		}
	}
	if err != nil {
		return toolResult("", errcode.InvalidArgument.New(err.Error()))
	}
	// 2. 摘要优先展示触发规则，完整提示词仍作为任务参数保存。
	var trigger string
	switch {
	case input.At != "":
		trigger = "at: " + input.At
	case input.Interval != 0:
		trigger = "interval_seconds: " + strconv.FormatInt(input.Interval, 10)
	case input.Cron != "":
		trigger = "cron: " + input.Cron
		if input.Timezone != "" {
			trigger += "\ntimezone: " + input.Timezone
		}
	}
	if err := toolapi.Confirm(c.Context(), "agent/schedule_task", taskSummary(input.Prompt, trigger), toolapi.Unspecified); err != nil {
		return toolResult("", err)
	}
	args := AgentTask{Creator: taskCreator{Instance: c.Instance(), User: c.User()}, Target: currentTaskTarget(c), Prompt: input.Prompt, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	for _, capability := range c.Event().Capabilities {
		if capability.Name == "message.send" && capability.Supported && capability.TextLimit != nil {
			args.Target.TextLimit = capability.TextLimit.Max
			args.Target.TextUnit = capability.TextLimit.Unit
		}
	}
	var key string
	switch {
	case input.At != "":
		key, err = agentTask.At(c.Context(), at, args)
	case input.Interval != 0:
		key, err = agentTask.Every(c.Context(), interval, args)
	case input.Cron != "":
		var options []taskapi.Option
		if input.Timezone != "" {
			options = append(options, taskapi.Timezone(input.Timezone))
		}
		key, err = agentTask.Cron(c.Context(), input.Cron, args, options...)
	}
	if err != nil {
		return toolResult("", err)
	}
	return taskJSON(struct {
		ID string `json:"id"`
	}{key})
}

func taskSummary(prompt, trigger string) string {
	summary := trigger + "\nprompt: " + prompt
	if len(summary) <= taskSummaryBytes {
		return summary
	}
	const omitted = "…"
	end := taskSummaryBytes - len(omitted)
	for !utf8.RuneStart(summary[end]) {
		end--
	}
	return summary[:end] + omitted
}

func listTasks(c *sdk.Context) string {
	instances, err := agentTask.List(c.Context())
	if err != nil {
		return toolResult("", err)
	}
	type listedTask struct {
		ID       string     `json:"id"`
		Prompt   string     `json:"prompt"`
		At       *time.Time `json:"at,omitempty"`
		Interval int64      `json:"interval_seconds,omitempty"`
		Cron     string     `json:"cron,omitempty"`
		Timezone string     `json:"timezone,omitempty"`
		NextAt   *time.Time `json:"next_at"`
	}
	tasks := []listedTask{}
	target := currentTaskTarget(c)
	for _, instance := range instances {
		if !instance.Args.Target.sameChat(target) {
			continue
		}
		task := listedTask{ID: instance.Key, Prompt: instance.Args.Prompt, Interval: int64(instance.Interval / time.Second), Cron: instance.Cron, Timezone: instance.Timezone, NextAt: instance.NextAt}
		if !instance.At.IsZero() {
			task.At = &instance.At
		}
		tasks = append(tasks, task)
	}
	return taskJSON(tasks)
}

func cancelTask(c *sdk.Context, arguments json.RawMessage) string {
	if !c.Has(sdk.Admin) {
		return "error: PERMISSION_DENIED"
	}
	var input struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return toolResult("", errcode.InvalidArgument.New(err.Error()))
	}
	if input.ID == "" {
		return toolResult("", errcode.InvalidArgument.New("id is required"))
	}
	if err := toolapi.Confirm(c.Context(), "agent/cancel_task", string(arguments), toolapi.Unspecified); err != nil {
		return toolResult("", err)
	}
	instances, err := agentTask.List(c.Context())
	if err != nil {
		return toolResult("", err)
	}
	for _, instance := range instances {
		if instance.Key == input.ID && instance.Args.Target.sameChat(currentTaskTarget(c)) {
			err := agentTask.Cancel(c.Context(), input.ID)
			if errcode.Of(err) == errcode.NotFound {
				return "error: NOT_FOUND"
			}
			if err != nil {
				return toolResult("", err)
			}
			return "{}"
		}
	}
	return "error: NOT_FOUND"
}

func taskJSON(value any) string {
	encoded, err := json.Marshal(value)
	return toolResult(string(encoded), err)
}
