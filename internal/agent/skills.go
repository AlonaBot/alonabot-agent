package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"github.com/AlonaBot/alonabot-agent/provider"
	"go.yaml.in/yaml/v3"
)

const (
	skillFileName         = "SKILL.md"
	skillFileLimit        = 256 << 10
	skillNameLimit        = 64
	skillDescriptionLimit = 1024 // Unicode 码点数。
	skillDelimiter        = "---\n"
	loadSkillToolName     = "load_skill"
)

type skill struct {
	name, description, body string
}

type skillProblem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func readSkills(directory string) ([]skill, []skillProblem) {
	skills, problems := []skill{}, []skillProblem{}
	if directory == "" {
		return skills, problems
	}
	// 1. 目录项按名字读取，只处理真实的一级子目录。
	root, err := os.OpenRoot(directory)
	if err != nil {
		slog.Warn("skills directory unavailable", "path", directory, "error", err)
		return skills, []skillProblem{{Path: directory, Message: err.Error()}}
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		slog.Warn("skills directory unavailable", "path", directory, "error", err)
		return skills, []skillProblem{{Path: directory, Message: err.Error()}}
	}
	names := make(map[string]bool)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(directory, entry.Name(), skillFileName)
		info, err := root.Lstat(entry.Name())
		var loaded skill
		var found bool
		if err == nil {
			loaded, found, err = readSkillDirectory(root, entry.Name(), info)
		}
		if err == nil && !found {
			continue
		}
		if err == nil && names[loaded.name] {
			err = fmt.Errorf("duplicate skill name %q", loaded.name)
		}
		if err != nil {
			slog.Warn("invalid skill file", "path", path, "error", err)
			problems = append(problems, skillProblem{Path: path, Message: err.Error()})
			continue
		}
		names[loaded.name] = true
		skills = append(skills, loaded)
	}
	// 2. 提示顺序只取决于技能名称，重复名称的优先级已由目录顺序确定。
	sort.Slice(skills, func(i, j int) bool { return skills[i].name < skills[j].name })
	return skills, problems
}

func readSkillDirectory(root *os.Root, name string, info os.FileInfo) (skill, bool, error) {
	if !info.IsDir() {
		return skill{}, false, fmt.Errorf("skill directory must be a directory")
	}
	directory, err := root.OpenRoot(name)
	if err != nil {
		return skill{}, false, err
	}
	defer directory.Close()
	opened, err := directory.Stat(".")
	if err != nil {
		return skill{}, false, err
	}
	if !opened.IsDir() || !os.SameFile(info, opened) {
		return skill{}, false, fmt.Errorf("skill directory changed while opening")
	}
	// 后续操作绑定已核对的目录句柄，目录改名或被替换时仍访问同一目录。
	files, err := fs.ReadDir(directory.FS(), ".")
	if err != nil {
		return skill{}, false, err
	}
	// 按目录项原名匹配，使大小写不敏感文件系统也跳过 skill.md。
	if !slices.ContainsFunc(files, func(file fs.DirEntry) bool { return file.Name() == skillFileName }) {
		return skill{}, false, nil
	}
	info, err = directory.Lstat(skillFileName)
	if err != nil {
		return skill{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return skill{}, false, nil
	}
	loaded, err := readSkillFile(directory, skillFileName, info)
	return loaded, true, err
}

func readSkillFile(directory *os.Root, path string, info os.FileInfo) (skill, error) {
	if !info.Mode().IsRegular() {
		return skill{}, fmt.Errorf("skill must be a regular file")
	}
	file, err := directory.Open(path)
	if err != nil {
		return skill{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return skill{}, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return skill{}, fmt.Errorf("skill file changed while opening")
	}
	if opened.Size() > skillFileLimit {
		return skill{}, fmt.Errorf("skill exceeds %d bytes", skillFileLimit)
	}
	// 文件在检查后增长时仍只读取上限加一字节，以识别超限而非截断成功。
	content, err := io.ReadAll(io.LimitReader(file, skillFileLimit+1))
	if err != nil {
		return skill{}, err
	}
	if len(content) > skillFileLimit {
		return skill{}, fmt.Errorf("skill exceeds %d bytes", skillFileLimit)
	}
	return parseSkill(content)
}

func parseSkill(content []byte) (skill, error) {
	// 1. 分隔符必须独占一行；正文保留分隔符之后的原文。
	frontmatter, ok := bytes.CutPrefix(content, []byte(skillDelimiter))
	if !ok {
		return skill{}, fmt.Errorf("missing frontmatter")
	}
	end := bytes.Index(frontmatter, []byte("\n"+skillDelimiter))
	if end < 0 {
		return skill{}, fmt.Errorf("missing closing frontmatter delimiter")
	}
	var metadata struct {
		Name        yaml.Node `yaml:"name"`
		Description yaml.Node `yaml:"description"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(frontmatter[:end+1]))
	if err := decoder.Decode(&metadata); err != nil {
		return skill{}, fmt.Errorf("decode frontmatter: %w", err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return skill{}, fmt.Errorf("decode frontmatter: %w", err)
		}
		return skill{}, fmt.Errorf("frontmatter must contain one YAML document")
	}
	// 2. 展开别名后保留 YAML 类型，避免数字和布尔值被自动转成字符串。
	nameNode := resolveSkillAlias(&metadata.Name)
	name := nameNode.Value
	if nameNode.Tag != "!!str" || len(name) == 0 || len(name) > skillNameLimit {
		return skill{}, fmt.Errorf("name must match [a-z0-9_-]{1,%d}", skillNameLimit)
	}
	for _, ch := range name {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return skill{}, fmt.Errorf("name must match [a-z0-9_-]{1,%d}", skillNameLimit)
		}
	}
	descriptionNode := resolveSkillAlias(&metadata.Description)
	description := descriptionNode.Value
	if descriptionNode.Tag != "!!str" || strings.TrimSpace(description) == "" || utf8.RuneCountInString(description) > skillDescriptionLimit {
		return skill{}, fmt.Errorf("description must contain 1 to %d characters", skillDescriptionLimit)
	}
	return skill{name: name, description: description, body: string(frontmatter[end+1+len(skillDelimiter):])}, nil
}

func resolveSkillAlias(node *yaml.Node) *yaml.Node {
	for node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	return node
}

func skillPrompt(skills []skill, header string) string {
	var prompt strings.Builder
	prompt.WriteString(header)
	for _, skill := range skills {
		prompt.WriteByte('\n')
		prompt.WriteString(skill.name + ": " + skill.description)
	}
	return prompt.String()
}

func addSkillTool(catalog *toolCatalog, skills []skill) {
	if len(skills) == 0 {
		return
	}
	tool := roundTool{local: func(arguments json.RawMessage) string { return loadSkill(skills, arguments) }, definition: provider.Tool{
		Name: loadSkillToolName, Description: "Load the full body of an available skill by name",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`),
	}}
	catalog.byName[loadSkillToolName] = tool
	catalog.definitions = append(catalog.definitions, tool.definition)
}

func loadSkill(skills []skill, arguments json.RawMessage) string {
	var input struct {
		Name *string `json:"name"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return toolResult("", errcode.InvalidArgument.New("invalid skill arguments: "+err.Error()))
	}
	if input.Name == nil {
		return toolResult("", errcode.InvalidArgument.New("name must be a string"))
	}
	for _, skill := range skills {
		if skill.name == *input.Name {
			return skill.body
		}
	}
	return "error: NOT_FOUND: unknown skill"
}

// skillSnapshot 返回只读切片；重新读取只替换引用，已开始的轮次保留原目录。
func (a *component) skillSnapshot() ([]skill, []skillProblem) {
	a.skillsMu.RLock()
	defer a.skillsMu.RUnlock()
	return a.skills, a.skillProblems
}

func (a *component) reloadSkills() {
	skills, problems := readSkills(a.config.skillsDir)
	a.skillsMu.Lock()
	a.skills, a.skillProblems = skills, problems
	a.skillsMu.Unlock()
}
