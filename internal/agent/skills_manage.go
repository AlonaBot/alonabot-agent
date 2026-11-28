package agent

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/errcode"
	"go.yaml.in/yaml/v3"
)

const (
	skillArchiveMaxBytes      = 10 << 20 // 压缩包原文上限 10 MiB。
	skillArchiveMaxTotalBytes = 20 << 20 // 解压后总字节上限 20 MiB。
	skillArchiveMaxFileBytes  = 2 << 20  // 单文件解压上限 2 MiB。
	skillArchiveMaxFiles      = 200      // 单次上传最多包含 200 个文件。
)

// readAllSkills 合并读取外部配置目录与页面托管目录下的全部技能。
func readAllSkills(externalDir, managedDir string) ([]skill, []skillProblem) {
	skills := []skill{}
	problems := []skillProblem{}
	names := make(map[string]bool)

	appendDir := func(dir string, managed bool, missingOK bool) {
		if dir == "" {
			return
		}
		if missingOK {
			if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
				return
			}
		}
		loaded, probs := readSkills(dir)
		problems = append(problems, probs...)
		for _, item := range loaded {
			if names[item.name] {
				problems = append(problems, skillProblem{
					Path:    filepath.Join(dir, item.name, skillFileName),
					Message: fmt.Sprintf("duplicate skill name %q", item.name),
				})
				continue
			}
			item.managed = managed
			names[item.name] = true
			skills = append(skills, item)
		}
	}

	appendDir(externalDir, false, false)
	appendDir(managedDir, true, true)
	sort.Slice(skills, func(i, j int) bool { return skills[i].name < skills[j].name })
	return skills, problems
}

func validSkillName(name string) bool {
	if len(name) == 0 || len(name) > skillNameLimit {
		return false
	}
	for _, ch := range name {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

// buildSkillMarkdown 构造带合法 YAML frontmatter 的 SKILL.md 内容并复用 parseSkill 校验。
func buildSkillMarkdown(name, description, body string) ([]byte, error) {
	name = strings.TrimSpace(name)
	description = strings.TrimSpace(description)
	if !validSkillName(name) {
		return nil, errcode.InvalidArgument.New(fmt.Sprintf("name must match [a-z0-9_-]{1,%d}", skillNameLimit))
	}
	if description == "" || utf8.RuneCountInString(description) > skillDescriptionLimit {
		return nil, errcode.InvalidArgument.New(fmt.Sprintf("description must contain 1 to %d characters", skillDescriptionLimit))
	}
	header, err := yaml.Marshal(struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}{Name: name, Description: description})
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString(skillDelimiter)
	out.Write(header)
	if len(header) > 0 && header[len(header)-1] != '\n' {
		out.WriteByte('\n')
	}
	out.WriteString(skillDelimiter)
	out.WriteString(body)
	content := out.Bytes()
	if len(content) > skillFileLimit {
		return nil, errcode.InvalidArgument.New(fmt.Sprintf("skill exceeds %d bytes", skillFileLimit))
	}
	if _, err := parseSkill(content); err != nil {
		return nil, errcode.InvalidArgument.New(err.Error())
	}
	return content, nil
}

func writeManagedSkill(managedDir, name, description, body string, allowOverwrite bool) error {
	if managedDir == "" {
		return errcode.InvalidArgument.New("managed skills directory is unavailable")
	}
	content, err := buildSkillMarkdown(name, description, body)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(managedDir, 0o755); err != nil {
		return err
	}
	targetDir := filepath.Join(managedDir, name)
	if _, err := os.Stat(targetDir); err == nil && !allowOverwrite {
		return errcode.AlreadyExists.New("skill already exists: " + name)
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(targetDir, ".SKILL.md.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	cleanupTemporary := true
	defer func() {
		if cleanupTemporary {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmpFile.Write(content); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(targetDir, skillFileName)); err != nil {
		return err
	}
	cleanupTemporary = false
	return nil
}

func deleteManagedSkill(managedDir, name string) error {
	if managedDir == "" {
		return errcode.InvalidArgument.New("managed skills directory is unavailable")
	}
	if !validSkillName(name) {
		return errcode.InvalidArgument.New("invalid skill name")
	}
	root, err := os.OpenRoot(managedDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errcode.NotFound.New("unknown skill")
		}
		return err
	}
	info, statErr := root.Lstat(name)
	root.Close()
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return errcode.NotFound.New("unknown skill")
		}
		return statErr
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errcode.InvalidArgument.New("target is not a managed skill directory")
	}
	return os.RemoveAll(filepath.Join(managedDir, name))
}

type archiveEntry struct {
	relativePath string
	data         []byte
}

func cleanArchiveRelPath(raw string) (string, bool) {
	if raw == "" || strings.ContainsRune(raw, 0) || strings.ContainsRune(raw, '\\') || strings.HasPrefix(raw, "/") {
		return "", false
	}
	for _, seg := range strings.Split(raw, "/") {
		if seg == ".." {
			return "", false
		}
	}
	normalized := path.Clean(raw)
	if normalized == "" || normalized == "." || strings.HasPrefix(normalized, "/") || strings.HasPrefix(normalized, "..") {
		return "", false
	}
	for _, seg := range strings.Split(normalized, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	// 忽略 macOS Finder 附带的 __MACOSX 元数据与隐藏 .DS_Store。
	if strings.HasPrefix(normalized, "__MACOSX/") || normalized == "__MACOSX" || path.Base(normalized) == ".DS_Store" {
		return "", true
	}
	return normalized, true
}

func readZipEntries(raw []byte) ([]archiveEntry, error) {
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, errcode.InvalidArgument.New("invalid zip archive: " + err.Error())
	}
	var entries []archiveEntry
	var totalBytes int64
	for _, file := range reader.File {
		mode := file.Mode()
		if mode&os.ModeSymlink != 0 {
			return nil, errcode.InvalidArgument.New("archive must not contain symlinks")
		}
		if file.FileInfo().IsDir() {
			continue
		}
		rel, ok := cleanArchiveRelPath(file.Name)
		if !ok {
			return nil, errcode.InvalidArgument.New("archive contains unsafe path: " + file.Name)
		}
		if rel == "" {
			continue
		}
		if len(entries) >= skillArchiveMaxFiles {
			return nil, errcode.InvalidArgument.New("archive exceeds maximum file count")
		}
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(rc, skillArchiveMaxFileBytes+1))
		rc.Close()
		if readErr != nil {
			return nil, readErr
		}
		if len(data) > skillArchiveMaxFileBytes {
			return nil, errcode.InvalidArgument.New("archive file exceeds size limit: " + rel)
		}
		totalBytes += int64(len(data))
		if totalBytes > skillArchiveMaxTotalBytes {
			return nil, errcode.InvalidArgument.New("archive uncompressed size exceeds limit")
		}
		entries = append(entries, archiveEntry{relativePath: rel, data: data})
	}
	return entries, nil
}

func readTarGzEntries(raw []byte) ([]archiveEntry, error) {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, errcode.InvalidArgument.New("invalid gzip archive: " + err.Error())
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var entries []archiveEntry
	var totalBytes int64
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errcode.InvalidArgument.New("invalid tar archive: " + err.Error())
		}
		switch header.Typeflag {
		case tar.TypeDir, tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			return nil, errcode.InvalidArgument.New("archive must contain only regular files and directories")
		}
		rel, ok := cleanArchiveRelPath(header.Name)
		if !ok {
			return nil, errcode.InvalidArgument.New("archive contains unsafe path: " + header.Name)
		}
		if rel == "" {
			continue
		}
		if len(entries) >= skillArchiveMaxFiles {
			return nil, errcode.InvalidArgument.New("archive exceeds maximum file count")
		}
		data, err := io.ReadAll(io.LimitReader(tr, skillArchiveMaxFileBytes+1))
		if err != nil {
			return nil, err
		}
		if len(data) > skillArchiveMaxFileBytes {
			return nil, errcode.InvalidArgument.New("archive file exceeds size limit: " + rel)
		}
		totalBytes += int64(len(data))
		if totalBytes > skillArchiveMaxTotalBytes {
			return nil, errcode.InvalidArgument.New("archive uncompressed size exceeds limit")
		}
		entries = append(entries, archiveEntry{relativePath: rel, data: data})
	}
	return entries, nil
}

// replaceSkillDirectory 将暂存目录发布到目标目录；覆盖已有目录时先移至备份目录，
// 新目录就位后再清理备份，发布失败时尝试将备份目录恢复原位。
func replaceSkillDirectory(stageDir, targetDir, managedDir, skillName string, overwrite bool, rename func(oldpath, newpath string) error) error {
	_, statErr := os.Stat(targetDir)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if exists && !overwrite {
		return errcode.AlreadyExists.New("skill already exists: " + skillName)
	}
	if !exists {
		return rename(stageDir, targetDir)
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	backupDir := filepath.Join(managedDir, ".skill-backup-"+skillName+"-"+hex.EncodeToString(suffix[:]))
	if err := rename(targetDir, backupDir); err != nil {
		return fmt.Errorf("backup existing skill directory: %w", err)
	}
	if err := rename(stageDir, targetDir); err != nil {
		if restoreErr := rename(backupDir, targetDir); restoreErr != nil {
			return errors.Join(fmt.Errorf("publish skill directory: %w", err), fmt.Errorf("restore skill backup: %w", restoreErr))
		}
		return fmt.Errorf("publish skill directory: %w", err)
	}
	if err := os.RemoveAll(backupDir); err != nil {
		return fmt.Errorf("remove skill backup: %w", err)
	}
	return nil
}

// installSkillArchive 校验并安全解压技能压缩包（.zip / .tar.gz / .tgz）至托管技能目录。
func installSkillArchive(managedDir, filename string, raw []byte, overwrite bool) (skill, error) {
	if managedDir == "" {
		return skill{}, errcode.InvalidArgument.New("managed skills directory is unavailable")
	}
	if len(raw) == 0 || len(raw) > skillArchiveMaxBytes {
		return skill{}, errcode.InvalidArgument.New("archive is empty or exceeds 10 MiB limit")
	}
	lower := strings.ToLower(strings.TrimSpace(filename))
	var entries []archiveEntry
	var err error
	switch {
	case strings.HasSuffix(lower, ".zip"):
		entries, err = readZipEntries(raw)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		entries, err = readTarGzEntries(raw)
	default:
		return skill{}, errcode.InvalidArgument.New("unsupported archive format; use .zip, .tar.gz, or .tgz")
	}
	if err != nil {
		return skill{}, err
	}
	if len(entries) == 0 {
		return skill{}, errcode.InvalidArgument.New("archive contains no files")
	}

	// 1. 若全部文件共享唯一顶层目录，则自动剥离该顶层包装目录。
	hasRootSkill := false
	topDirs := map[string]bool{}
	for _, entry := range entries {
		if entry.relativePath == skillFileName {
			hasRootSkill = true
		}
		if idx := strings.IndexByte(entry.relativePath, '/'); idx > 0 {
			topDirs[entry.relativePath[:idx]] = true
		} else {
			topDirs[""] = true
		}
	}
	if !hasRootSkill && len(topDirs) == 1 && !topDirs[""] {
		var prefix string
		for key := range topDirs {
			prefix = key + "/"
		}
		for i := range entries {
			entries[i].relativePath = strings.TrimPrefix(entries[i].relativePath, prefix)
		}
	}

	// 2. 定位并校验 SKILL.md，提取规范技能名称。
	var skillData []byte
	for _, entry := range entries {
		if entry.relativePath == skillFileName {
			skillData = entry.data
			break
		}
	}
	if len(skillData) == 0 {
		return skill{}, errcode.InvalidArgument.New("archive is missing SKILL.md at root or top directory")
	}
	if len(skillData) > skillFileLimit {
		return skill{}, errcode.InvalidArgument.New(fmt.Sprintf("SKILL.md exceeds %d bytes", skillFileLimit))
	}
	parsed, err := parseSkill(skillData)
	if err != nil {
		return skill{}, errcode.InvalidArgument.New("invalid SKILL.md: " + err.Error())
	}

	// 3. 先落盘到同目录临时目录；覆盖已有目录时先移至备份目录，新目录就位后再清理备份，失败时恢复原目录。
	if err := os.MkdirAll(managedDir, 0o755); err != nil {
		return skill{}, err
	}
	targetDir := filepath.Join(managedDir, parsed.name)
	if _, err := os.Stat(targetDir); err == nil && !overwrite {
		return skill{}, errcode.AlreadyExists.New("skill already exists: " + parsed.name)
	}
	stageDir, err := os.MkdirTemp(managedDir, ".skill-upload-*")
	if err != nil {
		return skill{}, err
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = os.RemoveAll(stageDir)
		}
	}()
	for _, entry := range entries {
		dest := filepath.Join(stageDir, filepath.FromSlash(entry.relativePath))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return skill{}, err
		}
		if err := os.WriteFile(dest, entry.data, 0o644); err != nil {
			return skill{}, err
		}
	}
	if err := replaceSkillDirectory(stageDir, targetDir, managedDir, parsed.name, overwrite, os.Rename); err != nil {
		// 若仅在删除备份时出错，新目录已经就位，返回部分成功结果与原始错误。
		if _, statErr := os.Stat(stageDir); errors.Is(statErr, os.ErrNotExist) {
			cleanupStage = false
			parsed.managed = true
			return parsed, err
		}
		return skill{}, err
	}
	cleanupStage = false
	parsed.managed = true
	return parsed, nil
}

type pageSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Managed     bool   `json:"managed"`
}

func (a *component) skillsPageResponse() any {
	skills, problems := a.skillSnapshot()
	listed := make([]pageSkill, 0, len(skills))
	for _, entry := range skills {
		listed = append(listed, pageSkill{
			Name:        entry.name,
			Description: entry.description,
			Body:        entry.body,
			Managed:     entry.managed,
		})
	}
	return struct {
		Skills   []pageSkill    `json:"skills"`
		Problems []skillProblem `json:"problems"`
	}{listed, problems}
}

func (a *component) ensureManagedSkillsDir(ctx context.Context) string {
	a.skillsMu.RLock()
	loaded := a.managedSkillsLoaded
	dir := a.managedSkillsDir
	a.skillsMu.RUnlock()
	if loaded {
		return dir
	}
	if dataDir := sdk.PluginDataDir(ctx); dataDir != "" {
		dir = filepath.Join(dataDir, "skills")
	}
	skills, problems := readAllSkills(a.config.skillsDir, dir)
	a.skillsMu.Lock()
	if !a.managedSkillsLoaded {
		a.managedSkillsDir = dir
		a.managedSkillsLoaded = true
		a.skills, a.skillProblems = skills, problems
	}
	result := a.managedSkillsDir
	a.skillsMu.Unlock()
	return result
}

// skillSnapshotFor 在 Session 激活后首次访问时惰性绑定托管技能目录并返回只读技能快照。
func (a *component) skillSnapshotFor(ctx context.Context) ([]skill, []skillProblem) {
	a.ensureManagedSkillsDir(ctx)
	return a.skillSnapshot()
}

func (a *component) pageSkillsManage(r *http.Request) (any, int, error) {
	if r.Method == http.MethodGet {
		a.ensureManagedSkillsDir(r.Context())
		return a.skillsPageResponse(), http.StatusOK, nil
	}
	// 串行化托管技能的写入、覆盖、删除与内存重读，避免并发争用临时文件或快照倒退。
	a.skillsManageMu.Lock()
	defer a.skillsManageMu.Unlock()
	managedDir := a.ensureManagedSkillsDir(r.Context())
	switch r.Method {
	case http.MethodPost, http.MethodPut:
		var input struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Body        string `json:"body"`
		}
		if err := decodePageRequest(r, &input); err != nil {
			return nil, 0, err
		}
		if r.Method == http.MethodPut {
			// 只允许覆盖已存在的托管技能，禁止覆盖外部只读技能。
			skills, _ := a.skillSnapshot()
			foundManaged := false
			for _, item := range skills {
				if item.name == strings.TrimSpace(input.Name) && item.managed {
					foundManaged = true
					break
				}
			}
			if !foundManaged {
				return nil, 0, errcode.NotFound.New("unknown managed skill")
			}
		} else {
			skills, _ := a.skillSnapshot()
			for _, item := range skills {
				if item.name == strings.TrimSpace(input.Name) {
					return nil, 0, errcode.AlreadyExists.New("skill already exists: " + item.name)
				}
			}
		}
		if err := writeManagedSkill(managedDir, strings.TrimSpace(input.Name), input.Description, input.Body, r.Method == http.MethodPut); err != nil {
			return nil, 0, err
		}
		a.reloadSkills()
		status := http.StatusOK
		if r.Method == http.MethodPost {
			status = http.StatusCreated
		}
		return a.skillsPageResponse(), status, nil

	case http.MethodDelete:
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if err := deleteManagedSkill(managedDir, name); err != nil {
			return nil, 0, err
		}
		a.reloadSkills()
		return a.skillsPageResponse(), http.StatusOK, nil
	}
	return nil, 0, errcode.InvalidArgument.New("unsupported method")
}

func (a *component) pageSkillsUpload(r *http.Request) (any, int, error) {
	var input struct {
		Filename   string `json:"filename"`
		DataBase64 string `json:"data_base64"`
		Overwrite  bool   `json:"overwrite"`
	}
	if err := decodePageRequest(r, &input); err != nil {
		return nil, 0, err
	}
	if strings.TrimSpace(input.Filename) == "" || strings.TrimSpace(input.DataBase64) == "" {
		return nil, 0, errcode.InvalidArgument.New("filename and data_base64 are required")
	}
	raw, err := base64.StdEncoding.DecodeString(input.DataBase64)
	if err != nil {
		return nil, 0, errcode.InvalidArgument.New("invalid base64 archive data")
	}
	a.skillsManageMu.Lock()
	defer a.skillsManageMu.Unlock()
	managedDir := a.ensureManagedSkillsDir(r.Context())
	installed, err := installSkillArchive(managedDir, input.Filename, raw, input.Overwrite)
	if installed.name != "" {
		a.reloadSkills()
	}
	if err != nil {
		return nil, 0, err
	}
	return a.skillsPageResponse(), http.StatusCreated, nil
}
