package importcompare

import (
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

var connectionKeys = []string{"host", "port", "dbname", "user", "password", "sslmode", "sslrootcert", "sslcert", "sslkey", "connect_timeout"}

func ParseDSN(raw string) (connectionSettings, error) {
	bad := func() (connectionSettings, error) { return nil, configFailure() }
	if len(raw) == 0 || len(raw) > 8192 || !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		return bad()
	}
	s := connectionSettings{}
	add := func(k, v string) bool {
		allowed := false
		for _, key := range connectionKeys {
			allowed = allowed || k == key
		}
		if _, ok := s[k]; ok || !allowed || strings.ContainsRune(v, 0) {
			return false
		}
		s[k] = v
		return true
	}
	if strings.HasPrefix(raw, "postgres://") || strings.HasPrefix(raw, "postgresql://") {
		u, err := url.Parse(raw)
		if err != nil || u.Fragment != "" || u.Opaque != "" {
			return bad()
		}
		if u.User != nil {
			if !add("user", u.User.Username()) {
				return bad()
			}
			if pw, ok := u.User.Password(); ok {
				if !add("password", pw) {
					return bad()
				}
			}
		}
		if u.Host != "" {
			if !add("host", u.Hostname()) {
				return bad()
			}
			if u.Port() != "" {
				if !add("port", u.Port()) {
					return bad()
				}
			}
		}
		if u.Path != "" {
			if !add("dbname", strings.TrimPrefix(u.Path, "/")) {
				return bad()
			}
		}
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return bad()
		}
		for k, values := range q {
			if len(values) != 1 || !add(k, values[0]) {
				return bad()
			}
		}
	} else {
		whitespace := func(b byte) bool { return b == ' ' || b == '\t' || b == '\r' || b == '\n' }
		for pos := 0; pos < len(raw); {
			for pos < len(raw) && whitespace(raw[pos]) {
				pos++
			}
			if pos == len(raw) {
				break
			}
			start := pos
			for pos < len(raw) && raw[pos] != '=' && !whitespace(raw[pos]) {
				pos++
			}
			k := raw[start:pos]
			for pos < len(raw) && whitespace(raw[pos]) {
				pos++
			}
			if k == "" || pos == len(raw) || raw[pos] != '=' {
				return bad()
			}
			pos++
			for pos < len(raw) && whitespace(raw[pos]) {
				pos++
			}
			var v strings.Builder
			quoted := pos < len(raw) && raw[pos] == '\''
			if quoted {
				pos++
			}
			closed := !quoted
			for pos < len(raw) {
				b := raw[pos]
				if b == '\\' {
					pos++
					if pos == len(raw) {
						return bad()
					}
					v.WriteByte(raw[pos])
					pos++
					continue
				}
				if quoted && b == '\'' {
					pos++
					closed = true
					break
				}
				if !quoted && whitespace(b) {
					break
				}
				v.WriteByte(b)
				pos++
			}
			if !closed || quoted && pos < len(raw) && !whitespace(raw[pos]) || !add(k, v.String()) {
				return bad()
			}
		}
	}
	for _, k := range []string{"host", "dbname", "user", "sslmode"} {
		if s[k] == "" {
			return bad()
		}
	}
	host := s["host"]
	socket := strings.HasPrefix(host, "/")
	if strings.Contains(host, ",") || strings.ContainsAny(host, "\r\n\t") || !socket && strings.ContainsAny(host, " /\\") {
		return bad()
	}
	if s["port"] == "" {
		if !socket {
			return bad()
		}
		s["port"] = "5432"
	}
	decimal := func(v string, min, max int) bool {
		if v == "" {
			return false
		}
		for _, c := range v {
			if c < '0' || c > '9' {
				return false
			}
		}
		n, err := strconv.Atoi(v)
		return err == nil && n >= min && n <= max
	}
	if !decimal(s["port"], 1, 65535) {
		return bad()
	}
	if s["sslmode"] != "verify-full" && !(socket && s["sslmode"] == "disable") {
		return bad()
	}
	if v, exists := s["connect_timeout"]; exists {
		if !decimal(v, 1, 3) {
			return bad()
		}
	} else {
		s["connect_timeout"] = "3"
	}
	return s, nil
}
func (s connectionSettings) canonical() string {
	out := []string{}
	escape := strings.NewReplacer("\\", "\\\\", "'", "\\'")
	for _, k := range connectionKeys {
		if v, ok := s[k]; ok {
			out = append(out, k+"='"+escape.Replace(v)+"'")
		}
	}
	return strings.Join(out, " ")
}
