package importcompare

import (
	"context"
	"encoding/json"
	"fmt"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
)

type Stage string
type Issue struct {
	Stage Stage
	Issue p.Issue
}

func (i Issue) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(i.Issue)
	if err != nil {
		return nil, err
	}
	return append(append([]byte(fmt.Sprintf(`{"stage":%q,`, i.Stage)), b[1:len(b)-1]...), '}'), nil
}

type Classification struct {
	New       *int `json:"new"`
	Identical *int `json:"identical"`
	Conflict  *int `json:"conflict"`
}
type Classifications map[string]Classification
type Report struct {
	ReportVersion                  int             `json:"report_version"`
	ValidationProfile              string          `json:"validation_profile"`
	Scope                          string          `json:"scope"`
	Status                         p.Status        `json:"status"`
	FileStatus                     p.Status        `json:"file_status"`
	FileChecksComplete             bool            `json:"file_checks_complete"`
	ChecksComplete                 bool            `json:"checks_complete"`
	DatabaseChecked                bool            `json:"database_checked"`
	InputSHA256                    *string         `json:"input_sha256"`
	Counts                         p.Counts        `json:"counts"`
	ClassificationCounts           Classifications `json:"classification_counts"`
	ErrorsTotal                    int             `json:"errors_total"`
	Issues                         []Issue         `json:"issues"`
	IssuesTruncated                bool            `json:"issues_truncated"`
	IdentityProviderChecked        bool            `json:"identity_provider_checked"`
	AdditionalDatabaseRulesChecked bool            `json:"additional_database_rules_checked"`
	WriteConcurrencyChecked        bool            `json:"write_concurrency_checked"`
	ImportAuthorized               bool            `json:"import_authorized"`
}
type RowRef struct {
	Entity p.Entity
	Row    int
}
type Snapshot struct {
	TenantFound bool
	Data        p.Document
	GlobalKeys  map[RowRef]bool
	SQLCount    int
}

// A snapshot is transient private data, never an output artifact.
func (Snapshot) MarshalJSON() ([]byte, error) { return nil, p.Failure{Code: "DATABASE_READ_FAILED"} }

type SnapshotReader interface {
	Read(context.Context, string, p.Document) (Snapshot, error)
}

func names() []string {
	out := []string{}
	for _, s := range p.Schema() {
		out = append(out, string(s.Entity))
	}
	return append(out, "total")
}
func BuildReport(file p.Report, status p.Status, complete bool, classes Classifications, c *Collector) Report {
	if c == nil {
		c = NewCollector()
	}
	counts := p.Counts{}
	cl := Classifications{}
	for _, k := range names() {
		counts[k] = file.Counts[k]
		if complete {
			cl[k] = classes[k]
		} else {
			cl[k] = Classification{}
		}
	}
	fs := file.Status
	if fs == "" {
		fs = "incomplete"
	}
	issues := c.Issues()
	if status == "valid" && (c.Total() > 0 || !complete) {
		status = "invalid"
	}
	return Report{ReportVersion: 1, ValidationProfile: "group_identity_database_v1", Scope: "database_snapshot_insert_compatibility", Status: status, FileStatus: fs, FileChecksComplete: file.ChecksComplete, ChecksComplete: complete, DatabaseChecked: complete, InputSHA256: file.InputSHA256, Counts: counts, ClassificationCounts: cl, ErrorsTotal: c.Total(), Issues: issues, IssuesTruncated: c.Total() > len(issues)}
}
func FileIssues(file p.Report) *Collector {
	c := NewCollector()
	for _, i := range file.Issues {
		c.Add(Issue{"file", i})
	}
	c.fileHidden = file.ErrorsTotal - len(file.Issues)
	return c
}
func Incomplete(file p.Report, stage Stage, code p.Code) Report {
	c := FileIssues(file)
	c.Add(Issue{stage, p.Issue{Entity: "document", Field: "_document", Code: code}})
	return BuildReport(file, "incomplete", false, nil, c)
}
func (r Report) ExitCode() int {
	if r.Status == "valid" && r.ChecksComplete && r.ErrorsTotal == 0 {
		return 0
	}
	if r.Status == "invalid" {
		return 1
	}
	return 2
}
func EncodeReport(r Report) ([]byte, error) {
	if err := validateReport(r); err != nil {
		return nil, err
	}
	b, err := json.Marshal(r)
	if err != nil || len(b)+1 > p.MaxReport {
		return nil, p.Failure{Code: "OUTPUT_WRITE_FAILED"}
	}
	return append(b, '\n'), nil
}
