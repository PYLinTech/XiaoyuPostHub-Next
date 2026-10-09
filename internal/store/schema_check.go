package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
	"unicode"
)

// logSchemaDifferences 只提示结构差异；校验失败也不会改变正常启动流程。
func (db *DB) logSchemaDifferences() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	differences, err := db.schemaDifferences(ctx)
	if err != nil {
		log.Printf("store: 警告：schema.sql 结构校验未完成，继续启动: %v", err)
		return
	}
	for _, difference := range differences {
		log.Printf("store: 警告：数据库与 schema.sql 不一致，继续启动: %s", difference)
	}
}

// schemaDifferences 用隔离的内存库解析 schema.sql，业务库只执行查询。
func (db *DB) schemaDifferences(ctx context.Context) ([]string, error) {
	reference, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("创建结构参考库失败: %w", err)
	}
	defer reference.Close()
	reference.SetMaxOpenConns(1)
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return nil, err
	}
	for _, stmt := range splitStatements(string(schema)) {
		if _, err := reference.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("解析 schema.sql 失败: %w", err)
		}
	}
	want, err := schemaDefinitions(ctx, reference)
	if err != nil {
		return nil, err
	}
	got, err := schemaDefinitions(ctx, db.read)
	if err != nil {
		return nil, err
	}
	var differences []string
	for key, definition := range want {
		actual, exists := got[key]
		if !exists {
			differences = append(differences, "缺少 "+key)
		} else if actual != definition {
			differences = append(differences, "定义不一致 "+key)
		}
	}
	for key := range got {
		if _, exists := want[key]; !exists {
			differences = append(differences, "额外对象 "+key)
		}
	}
	sort.Strings(differences)
	return differences, nil
}

func schemaDefinitions(ctx context.Context, q Querier) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT type, name, sql FROM sqlite_schema
		WHERE name NOT GLOB 'sqlite_*' AND sql IS NOT NULL ORDER BY type, name`)
	if err != nil {
		return nil, fmt.Errorf("读取数据库结构失败: %w", err)
	}
	defer rows.Close()
	definitions := make(map[string]string)
	for rows.Next() {
		var kind, name, ddl string
		if err := rows.Scan(&kind, &name, &ddl); err != nil {
			return nil, err
		}
		canonical, err := canonicalSchemaSQL(kind, ddl)
		if err != nil {
			return nil, fmt.Errorf("解析 %s %s 失败: %w", kind, name, err)
		}
		definitions[kind+" "+name] = canonical
	}
	return definitions, rows.Err()
}

// canonicalSchemaSQL 忽略注释、空白、标识符引用、IF NOT EXISTS 与表的列顺序。
// 字符串值、默认值、约束及索引/复合键的列顺序都参与比较。
func canonicalSchemaSQL(kind, ddl string) (string, error) {
	tokens, err := schemaSQLTokens(ddl)
	if err != nil {
		return "", err
	}
	if len(tokens) > 5 && tokens[2] == "IF" && tokens[3] == "NOT" && tokens[4] == "EXISTS" {
		tokens = append(tokens[:2], tokens[5:]...)
	}
	if len(tokens) > 6 && tokens[1] == "UNIQUE" && tokens[3] == "IF" && tokens[4] == "NOT" && tokens[5] == "EXISTS" {
		tokens = append(tokens[:3], tokens[6:]...)
	}
	if kind != "table" {
		return strings.Join(tokens, " "), nil
	}
	first := -1
	for i, token := range tokens {
		if token == "(" {
			first = i
			break
		}
	}
	if first < 0 {
		return "", fmt.Errorf("表定义缺少列清单")
	}
	var clauses []string
	start, depth := first+1, 1
	for i := start; i < len(tokens); i++ {
		switch tokens[i] {
		case "(":
			depth++
		case ")":
			depth--
		}
		if depth == 0 || (depth == 1 && tokens[i] == ",") {
			clauses = append(clauses, strings.Join(tokens[start:i], " "))
			start = i + 1
		}
		if depth == 0 {
			sort.Strings(clauses)
			return strings.Join(tokens[:first], " ") + " ( " + strings.Join(clauses, " , ") + " ) " + strings.Join(tokens[i+1:], " "), nil
		}
	}
	return "", fmt.Errorf("表定义括号不完整")
}

func schemaSQLTokens(ddl string) ([]string, error) {
	runes := []rune(ddl)
	var tokens []string
	for i := 0; i < len(runes); {
		r := runes[i]
		if unicode.IsSpace(r) {
			i++
			continue
		}
		if r == '-' && i+1 < len(runes) && runes[i+1] == '-' {
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			continue
		}
		if r == '/' && i+1 < len(runes) && runes[i+1] == '*' {
			i += 2
			for i+1 < len(runes) && !(runes[i] == '*' && runes[i+1] == '/') {
				i++
			}
			if i+1 >= len(runes) {
				return nil, fmt.Errorf("注释未闭合")
			}
			i += 2
			continue
		}
		if r == '\'' || r == '"' || r == '`' || r == '[' {
			start, end := i, r
			if r == '[' {
				end = ']'
			}
			i++
			var value strings.Builder
			closed := false
			for i < len(runes) {
				if runes[i] == end {
					if end != ']' && i+1 < len(runes) && runes[i+1] == end {
						value.WriteRune(end)
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				value.WriteRune(runes[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("引用未闭合")
			}
			if r == '\'' {
				tokens = append(tokens, string(runes[start:i]))
			} else {
				tokens = append(tokens, strings.ToUpper(value.String()))
			}
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			start := i
			for i < len(runes) && (unicode.IsLetter(runes[i]) || unicode.IsDigit(runes[i]) || runes[i] == '_') {
				i++
			}
			tokens = append(tokens, strings.ToUpper(string(runes[start:i])))
			continue
		}
		tokens = append(tokens, string(r))
		i++
	}
	return tokens, nil
}
