package service

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

// 文档解析：把知识库可导入的范围从「只有 .md/.txt」扩到「PDF / DOCX / CSV」。
//
// 取舍说明：
//   - DOCX 本质是 zip + OOXML，用标准库 archive/zip + encoding/xml 解析，
//     零新增依赖，不存在引入第三方包带来的供应链风险。
//   - PDF 优先调用 poppler 的 pdftotext（Git for Windows 自带一份），
//     它处理中文 CID 字体与乱码顺序最稳；找不到时才退化到内置的
//     纯 Go 流解析器，并对「扫描件 / 无 ToUnicode」的 PDF 显式报错，
//     绝不把一屏乱码当成导入成功。

// SupportedDocExts 知识库可导入的扩展名（与前端提示共用）
var SupportedDocExts = []string{".md", ".markdown", ".txt", ".csv", ".docx", ".pdf"}

// IsSupportedDoc 判断扩展名是否可解析
func IsSupportedDoc(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, e := range SupportedDocExts {
		if ext == e {
			return true
		}
	}
	return false
}

// ExtractDocumentText 把文档字节转成纯文本（按扩展名分派）
func ExtractDocumentText(name string, data []byte) (string, error) {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".md", ".markdown", ".txt":
		return normalizeDocText(stripBOM(string(data))), nil
	case ".csv":
		return normalizeDocText(stripBOM(string(data))), nil
	case ".docx":
		return extractDOCX(data)
	case ".pdf":
		return extractPDF(data)
	case ".doc":
		return "", errors.New("暂不支持旧版 .doc（二进制格式），请用 Word 另存为 .docx 后再导入")
	default:
		return "", fmt.Errorf("不支持的文件类型 %s（支持：%s）", ext, strings.Join(SupportedDocExts, " "))
	}
}

func stripBOM(s string) string {
	return strings.TrimPrefix(s, "\uFEFF")
}

// normalizeDocText 收敛空白：保留段落，去掉行尾空格与连续空行噪音
func normalizeDocText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\u00a0", " ")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t")
		if strings.TrimSpace(ln) == "" {
			blank++
			if blank > 1 { // 最多保留一个空行
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// ===== DOCX =====

func extractDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("docx 不是合法的 zip 容器：%w", err)
	}
	target := "word/document.xml"
	for _, f := range zr.File {
		if f.Name == target {
			rc, err := f.Open()
			if err != nil {
				return "", err
			}
			defer rc.Close()
			txt, err := parseWordXML(rc)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(txt) == "" {
				return "", errors.New("docx 正文为空（可能全是图片或文本框，需 OCR）")
			}
			return txt, nil
		}
	}
	return "", errors.New("docx 内未找到 word/document.xml")
}

// parseWordXML 抽取 w:p / w:t 文本，保留段落与制表符
func parseWordXML(r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	var sb strings.Builder
	inText := false

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("解析 docx 正文失败：%w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				sb.WriteString("\t")
			case "br", "cr":
				sb.WriteString("\n")
			}
		case xml.CharData:
			if inText {
				sb.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				sb.WriteString("\n")
			case "tc": // 表格单元格之间用制表符分隔，避免多列粘成一句
				sb.WriteString("\t")
			}
		}
	}
	return normalizeDocText(sb.String()), nil
}

// ===== PDF =====

// pdfToTextCandidates pdftotext 的常见落位（PATH 之外再兜底几处）
func findPDFToText() string {
	if p, err := exec.LookPath("pdftotext"); err == nil {
		return p
	}
	candidates := []string{
		`C:\Program Files\Git\mingw64\bin\pdftotext.exe`,
		`C:\Program Files (x86)\Git\mingw64\bin\pdftotext.exe`,
		`D:\Git\mingw64\bin\pdftotext.exe`,
		`C:\Program Files\poppler\bin\pdftotext.exe`,
		`C:\poppler\bin\pdftotext.exe`,
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

func extractPDF(data []byte) (string, error) {
	if p := findPDFToText(); p != "" {
		if txt, err := runPDFToText(p, data); err == nil && strings.TrimSpace(txt) != "" {
			return normalizeDocText(txt), nil
		}
	}

	txt := normalizeDocText(pdfTextFromStreams(data))
	if strings.TrimSpace(txt) == "" || printableRatio(txt) < 0.55 {
		return "", errors.New("该 PDF 提取不到有效文本（扫描件，或使用了未带 ToUnicode 映射的 CID 字体）。请先转成文本，或安装 poppler 的 pdftotext 后重试")
	}
	return txt, nil
}

// runPDFToText 落临时文件后调用 poppler（不依赖 shell 引号转义）
func runPDFToText(bin string, data []byte) (string, error) {
	dir, err := os.MkdirTemp("", "flowedge-pdf-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	in := filepath.Join(dir, "in.pdf")
	out := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(in, data, 0600); err != nil {
		return "", err
	}
	cmd := exec.Command(bin, "-enc", "UTF-8", "-q", in, out)
	if err := cmd.Run(); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return "", err
	}
	return stripBOM(string(raw)), nil
}

var pdfStreamRe = regexp.MustCompile(`(?s)stream\r?\n?(.*?)endstream`)

// pdfTextFromStreams 纯 Go 兜底：解压所有内容流并抽取文本操作符的字面量
func pdfTextFromStreams(data []byte) string {
	var sb strings.Builder

	for _, m := range pdfStreamRe.FindAllSubmatch(data, -1) {
		raw := bytes.TrimPrefix(m[1], []byte("\r\n"))
		raw = bytes.TrimPrefix(raw, []byte("\n"))
		raw = bytes.TrimSuffix(raw, []byte("\r\n"))
		raw = bytes.TrimSuffix(raw, []byte("\n"))

		content := raw
		if dec, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
			if plain, err := io.ReadAll(io.LimitReader(dec, 32<<20)); err == nil {
				content = plain
			}
			dec.Close()
		}
		sb.WriteString(extractPDFTextOps(content))
		sb.WriteString("\n")
	}
	// 未压缩的明文内容流（少数生成器直接写裸文本）
	sb.WriteString(extractPDFTextOps(data))
	return sb.String()
}

// extractPDFTextOps 扫描 (...) 与 <...> 字符串，跳过字典（<< >>）
func extractPDFTextOps(c []byte) string {
	var sb strings.Builder
	i := 0
	for i < len(c) {
		switch c[i] {
		case '(':
			s, n := readPDFLiteral(c[i:])
			if n > 0 {
				sb.WriteString(s)
				sb.WriteByte(' ')
				i += n
				continue
			}
		case '<':
			if i+1 < len(c) && c[i+1] == '<' {
				i += 2 // 字典开始，不是字符串
				continue
			}
			s, n := readPDFHexString(c[i:])
			if n > 0 {
				sb.WriteString(s)
				sb.WriteByte(' ')
				i += n
				continue
			}
		}
		i++
	}
	return sb.String()
}

// readPDFLiteral 读取 ( ... ) 字面量，返回文本与消耗字节数
func readPDFLiteral(b []byte) (string, int) {
	if len(b) == 0 || b[0] != '(' {
		return "", 0
	}
	var sb strings.Builder
	depth := 1
	i := 1
	for i < len(b) {
		ch := b[i]
		switch ch {
		case '\\':
			if i+1 >= len(b) {
				return sb.String(), i
			}
			esc := b[i+1]
			switch esc {
			case 'n':
				sb.WriteByte('\n')
				i += 2
			case 'r':
				sb.WriteByte('\r')
				i += 2
			case 't':
				sb.WriteByte('\t')
				i += 2
			case 'b':
				sb.WriteByte('\b')
				i += 2
			case 'f':
				sb.WriteByte('\f')
				i += 2
			case '(', ')', '\\':
				sb.WriteByte(esc)
				i += 2
			case '\r', '\n': // 续行
				i += 2
			default:
				if esc >= '0' && esc <= '7' {
					j := i + 1
					oct := 0
					for j < len(b) && j < i+4 && b[j] >= '0' && b[j] <= '7' {
						oct = oct*8 + int(b[j]-'0')
						j++
					}
					sb.WriteByte(byte(oct))
					i = j
				} else {
					sb.WriteByte(esc)
					i += 2
				}
			}
		case '(':
			depth++
			sb.WriteByte(ch)
			i++
		case ')':
			depth--
			if depth == 0 {
				return sb.String(), i + 1
			}
			sb.WriteByte(ch)
			i++
		default:
			sb.WriteByte(ch)
			i++
		}
	}
	return sb.String(), len(b)
}

// readPDFHexString 读取 <...> 十六进制串（含 UTF-16BE 识别）
func readPDFHexString(b []byte) (string, int) {
	end := bytes.IndexByte(b, '>')
	if end < 0 {
		return "", 0
	}
	hexStr := make([]byte, 0, end)
	for _, ch := range b[1:end] {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F') {
			hexStr = append(hexStr, ch)
		}
	}
	if len(hexStr)%2 == 1 {
		hexStr = append(hexStr, '0')
	}
	raw := make([]byte, len(hexStr)/2)
	for i := 0; i < len(raw); i++ {
		v, err := strconv.ParseUint(string(hexStr[i*2:i*2+2]), 16, 8)
		if err != nil {
			return "", end + 1
		}
		raw[i] = byte(v)
	}
	// UTF-16BE BOM
	if len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF {
		u := make([]uint16, 0, (len(raw)-2)/2)
		for i := 2; i+1 < len(raw); i += 2 {
			u = append(u, uint16(raw[i])<<8|uint16(raw[i+1]))
		}
		return string(utf16.Decode(u)), end + 1
	}
	return string(raw), end + 1
}

// printableRatio 可读字符占比（用于判定「提出来的是不是乱码」）
func printableRatio(s string) float64 {
	if s == "" {
		return 0
	}
	good, total := 0, 0
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			continue
		}
		total++
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			good++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(good) / float64(total)
}
