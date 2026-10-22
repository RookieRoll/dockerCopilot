package settings

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	maxStoredImageNameBytes   = 512
	maxStoredImageLogosBytes  = 4096
	defaultImageLogosPrefix   = "// 自定义镜像logo配置\nexport const customImageLogos = "
	defaultImageLogosSuffix   = ";\n"
	imageLogosExportTokenName = "customImageLogos"
)

// ErrImageLogosInvalid reports a mapping file that is not in the supported
// imageLogos.js format. Callers must keep the existing file untouched.
var ErrImageLogosInvalid = errors.New("image icon mapping is invalid")

type imageLogoEntry struct {
	Key   string
	Value string
}

type imageLogosDocument struct {
	prefix  []byte
	suffix  []byte
	entries []imageLogoEntry
}

func readImageLogosDocument(path string) (*imageLogosDocument, bool, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &imageLogosDocument{
			prefix: []byte(defaultImageLogosPrefix),
			suffix: []byte(defaultImageLogosSuffix),
		}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read image icon mapping: %w", err)
	}
	document, err := parseImageLogos(content)
	if err != nil {
		return nil, true, err
	}
	return document, true, nil
}

func parseImageLogos(content []byte) (*imageLogosDocument, error) {
	if !utf8.Valid(content) {
		return nil, imageLogosError(content, 0, "mapping file is not valid UTF-8")
	}
	tokens, err := tokenizeImageLogos(content)
	if err != nil {
		return nil, err
	}
	openIndex := -1
	exportCount := 0
	for i := 0; i+2 < len(tokens); i++ {
		if tokens[i].kind == jsTokenIdentifier && tokens[i].value == "export" &&
			tokens[i+1].kind == jsTokenIdentifier && tokens[i+1].value == "const" &&
			tokens[i+2].kind == jsTokenIdentifier && tokens[i+2].value == imageLogosExportTokenName {
			exportCount++
			if exportCount > 1 {
				return nil, imageLogosError(content, tokens[i].start, "multiple customImageLogos exports")
			}
			if i+4 >= len(tokens) || !isPunctuation(tokens[i+3], "=") || !isPunctuation(tokens[i+4], "{") {
				return nil, imageLogosError(content, tokens[i].start, "expected export const customImageLogos = { ... }")
			}
			openIndex = i + 4
		}
	}
	if openIndex == -1 {
		return nil, imageLogosError(content, 0, "expected export const customImageLogos = { ... }")
	}

	openToken := tokens[openIndex]
	entries := make([]imageLogoEntry, 0)
	seen := make(map[string]struct{})
	closeToken := jsToken{}
	index := openIndex + 1
	for {
		if index >= len(tokens) {
			return nil, imageLogosError(content, len(content), "missing closing brace")
		}
		if isPunctuation(tokens[index], "}") {
			closeToken = tokens[index]
			break
		}

		keyToken := tokens[index]
		if keyToken.kind != jsTokenString {
			return nil, imageLogosError(content, keyToken.start, "mapping key must be a quoted string")
		}
		if err := validateStoredMappingKey(keyToken.value); err != nil {
			return nil, imageLogosError(content, keyToken.start, "%v", err)
		}
		if _, duplicate := seen[keyToken.value]; duplicate {
			return nil, imageLogosError(content, keyToken.start, "duplicate mapping key %q", keyToken.value)
		}
		index++
		if index >= len(tokens) || !isPunctuation(tokens[index], ":") {
			return nil, imageLogosError(content, keyToken.end, "expected ':' after mapping key")
		}
		index++
		if index >= len(tokens) || tokens[index].kind != jsTokenString {
			return nil, imageLogosError(content, tokenOffset(tokens, index, len(content)), "mapping value must be a quoted string")
		}
		valueToken := tokens[index]
		if err := validateStoredMappingValue(valueToken.value); err != nil {
			return nil, imageLogosError(content, valueToken.start, "%v", err)
		}
		index++
		if index >= len(tokens) {
			return nil, imageLogosError(content, len(content), "missing closing brace")
		}
		if isPunctuation(tokens[index], ",") {
			seen[keyToken.value] = struct{}{}
			entries = append(entries, imageLogoEntry{Key: keyToken.value, Value: valueToken.value})
			index++
			continue
		}
		if isPunctuation(tokens[index], "}") {
			seen[keyToken.value] = struct{}{}
			entries = append(entries, imageLogoEntry{Key: keyToken.value, Value: valueToken.value})
			closeToken = tokens[index]
			break
		}
		return nil, imageLogosError(content, tokens[index].start, "expected ',' or '}' after mapping value")
	}

	return &imageLogosDocument{
		prefix:  append([]byte(nil), content[:openToken.start]...),
		suffix:  append([]byte(nil), content[closeToken.end:]...),
		entries: entries,
	}, nil
}

func (d *imageLogosDocument) set(key, value string) {
	for i := range d.entries {
		if d.entries[i].Key == key {
			d.entries[i].Value = value
			return
		}
	}
	d.entries = append(d.entries, imageLogoEntry{Key: key, Value: value})
}

func (d *imageLogosDocument) render() []byte {
	var buffer bytes.Buffer
	buffer.Write(d.prefix)
	buffer.WriteString("{\n")
	for _, entry := range d.entries {
		buffer.WriteString("  ")
		buffer.WriteString(quoteJSString(entry.Key))
		buffer.WriteString(": ")
		buffer.WriteString(quoteJSString(entry.Value))
		buffer.WriteString(",\n")
	}
	buffer.WriteString("}")
	buffer.Write(d.suffix)
	return buffer.Bytes()
}

func validateStoredMappingKey(key string) error {
	if key == "" {
		return errors.New("mapping key must not be empty")
	}
	if len(key) > maxStoredImageNameBytes {
		return fmt.Errorf("mapping key must be at most %d bytes", maxStoredImageNameBytes)
	}
	if !utf8.ValidString(key) {
		return errors.New("mapping key must be valid UTF-8")
	}
	for _, value := range key {
		if value < 0x20 || value == 0x7f {
			return errors.New("mapping key must not contain control characters")
		}
	}
	return nil
}

func validateStoredMappingValue(value string) error {
	if len(value) > maxStoredImageLogosBytes {
		return fmt.Errorf("mapping value must be at most %d bytes", maxStoredImageLogosBytes)
	}
	if !utf8.ValidString(value) {
		return errors.New("mapping value must be valid UTF-8")
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return errors.New("mapping value must not contain control characters")
		}
	}
	return nil
}

func quoteJSString(value string) string {
	var builder strings.Builder
	builder.Grow(len(value) + 2)
	builder.WriteByte('"')
	for _, char := range value {
		switch char {
		case '\\':
			builder.WriteString(`\\`)
		case '"':
			builder.WriteString(`\"`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			builder.WriteRune(char)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	return writeFileAtomicWithRename(path, content, mode, os.Rename)
}

func writeFileAtomicWithRename(path string, content []byte, mode os.FileMode, rename func(string, string) error) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporaryFile, err := os.CreateTemp(directory, ".icon-map-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporaryFile.Name()
	defer os.Remove(temporaryPath)

	if err := temporaryFile.Chmod(mode); err != nil {
		temporaryFile.Close()
		return err
	}
	if _, err := temporaryFile.Write(content); err != nil {
		temporaryFile.Close()
		return err
	}
	if err := temporaryFile.Sync(); err != nil {
		temporaryFile.Close()
		return err
	}
	if err := temporaryFile.Close(); err != nil {
		return err
	}
	return rename(temporaryPath, path)
}

func imageLogosError(content []byte, offset int, format string, args ...any) error {
	if offset < 0 {
		offset = 0
	}
	if offset > len(content) {
		offset = len(content)
	}
	line := 1 + bytes.Count(content[:offset], []byte{'\n'})
	lineStart := bytes.LastIndexByte(content[:offset], '\n') + 1
	column := offset - lineStart + 1
	return fmt.Errorf("%w at line %d column %d: %s", ErrImageLogosInvalid, line, column, fmt.Sprintf(format, args...))
}

func tokenOffset(tokens []jsToken, index, fallback int) int {
	if index >= 0 && index < len(tokens) {
		return tokens[index].start
	}
	return fallback
}

type jsTokenKind int

const (
	jsTokenIdentifier jsTokenKind = iota
	jsTokenString
	jsTokenPunctuation
	jsTokenTemplate
	jsTokenOther
)

type jsToken struct {
	kind  jsTokenKind
	value string
	start int
	end   int
}

func tokenizeImageLogos(content []byte) ([]jsToken, error) {
	tokens := make([]jsToken, 0, len(content)/8)
	for index := 0; index < len(content); {
		char := content[index]
		if isJSWhitespace(char) {
			index++
			continue
		}
		if char == '/' && index+1 < len(content) {
			switch content[index+1] {
			case '/':
				index += 2
				for index < len(content) && content[index] != '\n' {
					index++
				}
				continue
			case '*':
				end := bytes.Index(content[index+2:], []byte("*/"))
				if end == -1 {
					return nil, imageLogosError(content, index, "unterminated block comment")
				}
				index += end + 4
				continue
			}
		}
		if char == '"' || char == '\'' {
			value, end, err := scanJSString(content, index)
			if err != nil {
				return nil, imageLogosError(content, index, "%v", err)
			}
			tokens = append(tokens, jsToken{kind: jsTokenString, value: value, start: index, end: end})
			index = end
			continue
		}
		if char == '`' {
			end, err := scanJSTemplate(content, index)
			if err != nil {
				return nil, imageLogosError(content, index, "%v", err)
			}
			tokens = append(tokens, jsToken{kind: jsTokenTemplate, value: string(content[index:end]), start: index, end: end})
			index = end
			continue
		}
		if isJSIdentifierStart(char) {
			end := index + 1
			for end < len(content) && isJSIdentifierPart(content[end]) {
				end++
			}
			tokens = append(tokens, jsToken{kind: jsTokenIdentifier, value: string(content[index:end]), start: index, end: end})
			index = end
			continue
		}
		if strings.ContainsRune("{}[]()=:,;.-+*/%!?<>&|^~", rune(char)) {
			tokens = append(tokens, jsToken{kind: jsTokenPunctuation, value: string(char), start: index, end: index + 1})
			index++
			continue
		}
		_, size := utf8.DecodeRune(content[index:])
		if size == 0 {
			size = 1
		}
		tokens = append(tokens, jsToken{kind: jsTokenOther, value: string(content[index : index+size]), start: index, end: index + size})
		index += size
	}
	return tokens, nil
}

func scanJSString(content []byte, start int) (string, int, error) {
	quote := content[start]
	var builder strings.Builder
	for index := start + 1; index < len(content); {
		char := content[index]
		if char == quote {
			return builder.String(), index + 1, nil
		}
		if char == '\n' || char == '\r' {
			return "", 0, errors.New("unterminated string literal")
		}
		if char != '\\' {
			builder.WriteByte(char)
			index++
			continue
		}
		index++
		if index >= len(content) {
			return "", 0, errors.New("unterminated string escape")
		}
		escaped := content[index]
		switch escaped {
		case 'n':
			builder.WriteByte('\n')
		case 'r':
			builder.WriteByte('\r')
		case 't':
			builder.WriteByte('\t')
		case 'b':
			builder.WriteByte('\b')
		case 'f':
			builder.WriteByte('\f')
		case 'v':
			builder.WriteByte('\v')
		case '0':
			builder.WriteByte(0)
		case 'u':
			value, next, err := scanJSUnicodeEscape(content, index+1)
			if err != nil {
				return "", 0, err
			}
			if value >= 0xD800 && value <= 0xDBFF {
				if next+1 >= len(content) || content[next] != '\\' || content[next+1] != 'u' {
					return "", 0, errors.New("unpaired UTF-16 high surrogate escape")
				}
				low, afterLow, lowErr := scanJSUnicodeEscape(content, next+2)
				if lowErr != nil || low < 0xDC00 || low > 0xDFFF {
					return "", 0, errors.New("invalid UTF-16 surrogate pair")
				}
				builder.WriteRune(0x10000 + (value-0xD800)*0x400 + (low - 0xDC00))
				index = afterLow
				continue
			}
			if value >= 0xDC00 && value <= 0xDFFF {
				return "", 0, errors.New("unpaired UTF-16 low surrogate escape")
			}
			builder.WriteRune(value)
			index = next
			continue
		case 'x':
			if index+2 >= len(content) {
				return "", 0, errors.New("invalid hexadecimal escape")
			}
			value, ok := parseHexByte(content[index+1], content[index+2])
			if !ok {
				return "", 0, errors.New("invalid hexadecimal escape")
			}
			builder.WriteRune(rune(value))
			index += 3
			continue
		default:
			builder.WriteByte(escaped)
		}
		index++
	}
	return "", 0, errors.New("unterminated string literal")
}

func scanJSUnicodeEscape(content []byte, start int) (rune, int, error) {
	if start < len(content) && content[start] == '{' {
		end := bytes.IndexByte(content[start+1:], '}')
		if end == -1 {
			return 0, 0, errors.New("invalid unicode escape")
		}
		value, ok := parseHexRune(content[start+1 : start+1+end])
		if !ok || value >= 0xD800 && value <= 0xDFFF {
			return 0, 0, errors.New("invalid unicode escape")
		}
		return value, start + end + 2, nil
	}
	if start+4 > len(content) {
		return 0, 0, errors.New("invalid unicode escape")
	}
	value, ok := parseHexRune(content[start : start+4])
	if !ok {
		return 0, 0, errors.New("invalid unicode escape")
	}
	return value, start + 4, nil
}

func parseHexRune(value []byte) (rune, bool) {
	var result rune
	if len(value) == 0 || len(value) > 6 {
		return 0, false
	}
	for _, char := range value {
		result <<= 4
		switch {
		case char >= '0' && char <= '9':
			result += rune(char - '0')
		case char >= 'a' && char <= 'f':
			result += rune(char-'a') + 10
		case char >= 'A' && char <= 'F':
			result += rune(char-'A') + 10
		default:
			return 0, false
		}
	}
	if result > utf8.MaxRune {
		return 0, false
	}
	return result, true
}

func parseHexByte(high, low byte) (byte, bool) {
	value, ok := parseHexRune([]byte{high, low})
	return byte(value), ok
}

func scanJSTemplate(content []byte, start int) (int, error) {
	for index := start + 1; index < len(content); index++ {
		switch content[index] {
		case '\\':
			index++
		case '`':
			return index + 1, nil
		}
	}
	return 0, errors.New("unterminated template literal")
}

func isJSWhitespace(char byte) bool {
	switch char {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}

func isJSIdentifierStart(char byte) bool {
	return char == '_' || char == '$' || char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z'
}

func isJSIdentifierPart(char byte) bool {
	return isJSIdentifierStart(char) || char >= '0' && char <= '9'
}

func isPunctuation(token jsToken, value string) bool {
	return token.kind == jsTokenPunctuation && token.value == value
}
