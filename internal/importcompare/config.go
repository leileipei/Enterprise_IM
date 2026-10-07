package importcompare

import (
	"encoding/json"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"regexp"
)

type connectionSettings map[string]string
type Config struct {
	connection connectionSettings
	Schema     string
}

func configFailure() error { return p.Failure{Code: "DATABASE_CONFIG_INVALID"} }

var schemaName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func LoadConfig(getenv func(string) string) (Config, error) {
	s, err := ParseDSN(getenv("IM_IMPORT_COMPARE_DATABASE_URL"))
	if err != nil {
		return Config{}, err
	}
	schema := getenv("IM_IMPORT_COMPARE_SCHEMA")
	if schema == "" {
		schema = "public"
	}
	if !schemaName.MatchString(schema) {
		return Config{}, configFailure()
	}
	return Config{connection: s, Schema: schema}, nil
}
func (Config) String() string               { return "import comparison config (redacted)" }
func (Config) GoString() string             { return "import comparison config (redacted)" }
func (Config) MarshalJSON() ([]byte, error) { return nil, configFailure() }

var _ json.Marshaler = Config{}
