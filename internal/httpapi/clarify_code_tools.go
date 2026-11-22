package httpapi

import (
	"bufio"
	"bytes"
	"errors"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Read-only code tools for clarification on Codex. Codex reads files through
// its shell, and its read-only sandbox (bwrap) cannot run inside the platform
// container, so the shell stays off and these tools read the analysis
// directory instead. os.Root keeps every path, symlinks included, inside it.
const (
	clarifyCodeReadLines = 400
	clarifyCodeMaxOutput = 128 << 10
	clarifyCodeMaxList   = 500
	clarifyCodeMaxHits   = 200
	clarifyCodeMaxFile   = 1 << 20
)

func clarifyCodeToolDescriptors(s *AgentToolSession) []agentToolDescriptor {
	if s == nil || s.Mode != AgentToolClarify || s.clarifyCodeRoot() == "" {
		return nil
	}
	return []agentToolDescriptor{
		{Name: "code_list_files", Kind: "code", Description: "列出分析目录（代码仓库）中的文件和子目录，只读。path 为相对目录，默认根目录；pattern 为可选的文件名通配符，如 *.go。递归列出，最多 500 条，跳过 .git。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"pattern":{"type":"string"}},"additionalProperties":false}`)},
		{Name: "code_read_file", Kind: "code", Description: "读取分析目录中的一个文本文件，只读，返回带行号的内容。path 为相对路径；offset 为起始行（从 1 开始），limit 为行数，默认 400 行。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`)},
		{Name: "code_search", Kind: "code", Description: "在分析目录中按正则表达式（Go RE2 语法）搜索文本，只读，返回「文件:行号: 内容」。path 限定子目录，glob 限定文件名，如 *.ts。最多 200 条。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"glob":{"type":"string"}},"required":["pattern"],"additionalProperties":false}`)},
	}
}

func callClarifyCodeTool(root, name string, args map[string]any) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errAgentToolForbidden
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return "", fmt.Errorf("分析目录不可用")
	}
	defer dir.Close()
	fsys := dir.FS()
	text := func(key string) string {
		value, _ := args[key].(string)
		return strings.TrimSpace(value)
	}
	number := func(key string, fallback int) int {
		var n float64
		switch v := args[key].(type) {
		case json.Number:
			n, _ = v.Float64()
		case float64:
			n = v
		}
		if n >= 1 {
			return int(n)
		}
		return fallback
	}
	rel, err := clarifyCodePath(root, text("path"))
	if err != nil {
		return "", err
	}
	switch name {
	case "code_list_files":
		return clarifyCodeList(fsys, rel, text("pattern"))
	case "code_read_file":
		return clarifyCodeRead(fsys, rel, number("offset", 1), number("limit", clarifyCodeReadLines))
	case "code_search":
		return clarifyCodeSearch(fsys, rel, text("glob"), text("pattern"))
	}
	return "", errAgentToolUnknown
}

// clarifyCodePath turns a model-supplied path into an fs.FS path below root.
// An absolute path is accepted only when it names something inside root.
func clarifyCodePath(root, p string) (string, error) {
	if p == "" {
		return ".", nil
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		rel, err := filepath.Rel(root, filepath.FromSlash(p))
		if err != nil || !filepath.IsLocal(rel) {
			return "", fmt.Errorf("路径不在分析目录内：%s", p)
		}
		p = rel
	}
	p = path.Clean(filepath.ToSlash(p))
	if !fs.ValidPath(p) {
		return "", fmt.Errorf("路径不在分析目录内：%s", p)
	}
	return p, nil
}

func clarifyCodeList(fsys fs.FS, dir, pattern string) (string, error) {
	if pattern != "" {
		if _, err := path.Match(pattern, ""); err != nil {
			return "", fmt.Errorf("pattern 无效：%v", err)
		}
	}
	var out []string
	truncated := false
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		if pattern != "" {
			if ok, _ := path.Match(pattern, d.Name()); !ok {
				return nil
			}
		}
		if len(out) == clarifyCodeMaxList {
			truncated = true
			return fs.SkipAll
		}
		if d.IsDir() {
			p += "/"
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		return "", clarifyCodeError(err)
	}
	result := strings.Join(out, "\n")
	if len(out) == 0 {
		result = "（没有匹配的文件）"
	}
	if truncated {
		result += fmt.Sprintf("\n…（只显示前 %d 条，用 path 或 pattern 缩小范围）", clarifyCodeMaxList)
	}
	return result, nil
}

func clarifyCodeRead(fsys fs.FS, name string, offset, limit int) (string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return "", clarifyCodeError(err)
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil {
		return "", clarifyCodeError(err)
	} else if info.IsDir() {
		return "", fmt.Errorf("%s 是目录，请用 code_list_files", name)
	}
	reader := bufio.NewReader(f)
	if head, _ := reader.Peek(8 << 10); bytes.IndexByte(head, 0) >= 0 {
		return "", fmt.Errorf("%s 是二进制文件", name)
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), clarifyCodeMaxFile)
	var b strings.Builder
	line, last := 0, 0
	more := false
	for scanner.Scan() {
		line++
		if line < offset {
			continue
		}
		if line >= offset+limit || b.Len() >= clarifyCodeMaxOutput {
			more = true
			break
		}
		fmt.Fprintf(&b, "%d\t%s\n", line, scanner.Text())
		last = line
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("读取 %s 失败：%v", name, err)
	}
	if last == 0 {
		return fmt.Sprintf("（%s 共 %d 行，offset=%d 之后没有内容）", name, line, offset), nil
	}
	if more {
		fmt.Fprintf(&b, "…（未读完，用 offset=%d 继续）", last+1)
	}
	return b.String(), nil
}

func clarifyCodeSearch(fsys fs.FS, dir, glob, pattern string) (string, error) {
	if pattern == "" {
		return "", fmt.Errorf("pattern 不能为空")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("pattern 不是有效的正则表达式：%v", err)
	}
	if glob != "" {
		if _, err := path.Match(glob, ""); err != nil {
			return "", fmt.Errorf("glob 无效：%v", err)
		}
	}
	var hits []string
	truncated := false
	err = fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if glob != "" {
			if ok, _ := path.Match(glob, d.Name()); !ok {
				return nil
			}
		}
		if info, err := d.Info(); err != nil || !info.Mode().IsRegular() || info.Size() > clarifyCodeMaxFile {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil || bytes.IndexByte(data[:min(len(data), 8<<10)], 0) >= 0 {
			return nil
		}
		for i, text := range strings.Split(string(data), "\n") {
			if !re.MatchString(text) {
				continue
			}
			if len(hits) == clarifyCodeMaxHits {
				truncated = true
				return fs.SkipAll
			}
			hits = append(hits, fmt.Sprintf("%s:%d: %s", p, i+1, truncateRunes(strings.TrimRight(text, "\r"), 300)))
		}
		return nil
	})
	if err != nil {
		return "", clarifyCodeError(err)
	}
	if len(hits) == 0 {
		return "（没有匹配）", nil
	}
	result := strings.Join(hits, "\n")
	if truncated {
		result += fmt.Sprintf("\n…（只显示前 %d 条，用 path 或 glob 缩小范围）", clarifyCodeMaxHits)
	}
	return result, nil
}

// clarifyCodeError hides host paths: only the analysis-relative name is shown.
func clarifyCodeError(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		if os.IsNotExist(pathErr.Err) {
			return fmt.Errorf("%s 不存在", pathErr.Path)
		}
		return fmt.Errorf("无法读取 %s", pathErr.Path)
	}
	return fmt.Errorf("无法读取分析目录")
}
