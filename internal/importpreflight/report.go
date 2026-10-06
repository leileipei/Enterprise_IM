// Package importpreflight validates a closed, offline group identity snapshot.
package importpreflight

import (
	"context"
	"encoding/json"
)

type Failure struct{ Code Code }

func (f Failure) Error() string { return string(f.Code) }
func ContextFailure(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return Failure{"TIMEOUT"}
	}
	return Failure{"CANCELED"}
}

type Issue struct {
	Entity     Entity
	Row        int
	Field      Field
	Code       Code
	RelatedRow int
}

func (i Issue) MarshalJSON() ([]byte, error) {
	var row, related *int
	if i.Row > 0 {
		row = &i.Row
	}
	if i.RelatedRow > 0 {
		related = &i.RelatedRow
	}
	return json.Marshal(struct {
		Entity  Entity `json:"entity"`
		Row     *int   `json:"row"`
		Field   Field  `json:"field"`
		Code    Code   `json:"code"`
		Related *int   `json:"related_row,omitempty"`
	}{i.Entity, row, i.Field, i.Code, related})
}

type Counts map[string]*int
type Outcome struct {
	Status         Status
	ChecksComplete bool
	InputSHA256    *string
	Counts         Counts
}
type Report struct {
	ReportVersion           int     `json:"report_version"`
	ValidationProfile       string  `json:"validation_profile"`
	Status                  Status  `json:"status"`
	ChecksComplete          bool    `json:"checks_complete"`
	InputSHA256             *string `json:"input_sha256"`
	Counts                  Counts  `json:"counts"`
	ErrorsTotal             int     `json:"errors_total"`
	Issues                  []Issue `json:"issues"`
	IssuesTruncated         bool    `json:"issues_truncated"`
	Scope                   string  `json:"scope"`
	DatabaseChecked         bool    `json:"database_checked"`
	IdentityProviderChecked bool    `json:"identity_provider_checked"`
	ImportAuthorized        bool    `json:"import_authorized"`
}

func BuildReport(o Outcome, c *Collector) Report {
	counts := Counts{}
	for _, e := range entities {
		counts[string(e)] = o.Counts[string(e)]
	}
	counts["total"] = o.Counts["total"]
	issues := c.sorted()
	s := o.Status
	if s == "valid" && (c.total > 0 || !o.ChecksComplete) {
		s = "invalid"
	}
	if s != "valid" && s != "invalid" && s != "incomplete" {
		s = "incomplete"
	}
	return Report{ReportVersion: 1, ValidationProfile: "group_identity_v1", Status: s, ChecksComplete: o.ChecksComplete, InputSHA256: o.InputSHA256, Counts: counts, ErrorsTotal: c.total, Issues: issues, IssuesTruncated: c.total > len(issues), Scope: "offline_file"}
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
	b, e := json.Marshal(r)
	if e != nil || len(b)+1 > MaxReport {
		return nil, Failure{"OUTPUT_WRITE_FAILED"}
	}
	return append(b, '\n'), nil
}
