package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Check run conclusions Talooner writes. The set is deliberately short: policy
// outcomes are success and failure, and everything that is Talooner's own fault
// is neutral (actions.md, "Check run").
const (
	ConclusionSuccess = "success"
	ConclusionFailure = "failure"
	ConclusionNeutral = "neutral"
)

// Annotation levels, as the API spells them. The level is display only — an
// annotation at failure level on a neutral check run does not make the check
// run fail, which is what lets a broken ruleset be loud without blocking a
// merge.
const (
	LevelNotice  = "notice"
	LevelWarning = "warning"
	LevelFailure = "failure"
)

// maxAnnotations is the API's cap per request. More than that is sent as
// follow-up updates to the same check run.
const maxAnnotations = 50

// maxAnnotationBatches bounds the follow-up updates, so a decision carrying
// thousands of annotations costs a bounded number of calls rather than the
// whole job's rate limit.
const maxAnnotationBatches = 10

var conclusions = map[string]bool{
	ConclusionSuccess: true,
	ConclusionFailure: true,
	ConclusionNeutral: true,
}

var levels = map[string]bool{
	LevelNotice:  true,
	LevelWarning: true,
	LevelFailure: true,
}

// Annotation is one finding pinned to a line of a file in the repo. GitHub
// silently drops an annotation whose path is not part of the PR's diff, which
// is why the ruleset annotations point at the ruleset itself.
type Annotation struct {
	Path      string
	StartLine int
	EndLine   int
	Level     string
	Title     string
	Message   string
}

// CheckRun is the one check run a run writes, already decided. Name and HeadSHA
// together are its identity: one check run of that name per head sha, updated
// in place.
type CheckRun struct {
	Name       string
	HeadSHA    string
	Conclusion string
	Title      string
	Summary    string
	Text       string
	// DetailsURL is where "Details" points. Empty leaves GitHub's default,
	// which is the job that wrote the check run.
	DetailsURL  string
	Annotations []Annotation
}

func (cr CheckRun) validate() error {
	if strings.TrimSpace(cr.Name) == "" {
		return fmt.Errorf("check run needs a name")
	}
	if strings.TrimSpace(cr.HeadSHA) == "" {
		return fmt.Errorf("check run %s needs a head sha", cr.Name)
	}
	if !conclusions[cr.Conclusion] {
		return fmt.Errorf("check run %s has conclusion %q, want success, failure or neutral", cr.Name, cr.Conclusion)
	}
	// The API rejects an output without a title or summary, and a check run with
	// no words on it is one nobody can act on anyway.
	if strings.TrimSpace(cr.Title) == "" || strings.TrimSpace(cr.Summary) == "" {
		return fmt.Errorf("check run %s needs a title and a summary", cr.Name)
	}
	for i, a := range cr.Annotations {
		if err := a.validate(); err != nil {
			return fmt.Errorf("check run %s, annotation %d: %w", cr.Name, i, err)
		}
	}
	return nil
}

func (a Annotation) validate() error {
	if strings.TrimSpace(a.Path) == "" {
		return fmt.Errorf("annotation needs a path")
	}
	if a.StartLine <= 0 || a.EndLine <= 0 {
		return fmt.Errorf("annotation on %s needs positive line numbers, got %d-%d", a.Path, a.StartLine, a.EndLine)
	}
	if a.EndLine < a.StartLine {
		return fmt.Errorf("annotation on %s ends at line %d before it starts at %d", a.Path, a.EndLine, a.StartLine)
	}
	if !levels[a.Level] {
		return fmt.Errorf("annotation on %s has level %q, want notice, warning or failure", a.Path, a.Level)
	}
	if strings.TrimSpace(a.Message) == "" {
		return fmt.Errorf("annotation on %s needs a message", a.Path)
	}
	return nil
}

type annotationPayload struct {
	Path            string `json:"path"`
	StartLine       int    `json:"start_line"`
	EndLine         int    `json:"end_line"`
	AnnotationLevel string `json:"annotation_level"`
	Title           string `json:"title,omitempty"`
	Message         string `json:"message"`
}

type outputPayload struct {
	Title       string              `json:"title"`
	Summary     string              `json:"summary"`
	Text        string              `json:"text,omitempty"`
	Annotations []annotationPayload `json:"annotations,omitempty"`
}

type checkRunPayload struct {
	Name        string         `json:"name,omitempty"`
	HeadSHA     string         `json:"head_sha,omitempty"`
	Status      string         `json:"status"`
	Conclusion  string         `json:"conclusion"`
	CompletedAt string         `json:"completed_at"`
	DetailsURL  string         `json:"details_url,omitempty"`
	Output      *outputPayload `json:"output"`
}

// UpsertCheckRun writes cr at its head sha, updating the existing check run of
// that name rather than adding a second one. It returns the check run's id.
//
// Never duplicating is the point: a PR with thirty pushes and re-runs shows one
// talooner check with the current verdict, not thirty. The identity is
// {name, head sha}, so a new commit does get its own check run — a verdict
// belongs to the code it was computed from.
func (c *Client) UpsertCheckRun(ctx context.Context, owner, repo string, cr CheckRun) (int64, error) {
	if err := cr.validate(); err != nil {
		return 0, err
	}

	id, err := c.findCheckRun(ctx, owner, repo, cr.Name, cr.HeadSHA)
	if err != nil {
		return 0, err
	}

	batches := batchAnnotations(cr.Annotations)
	body := checkRunPayload{
		Status:      "completed",
		Conclusion:  cr.Conclusion,
		CompletedAt: c.now().UTC().Format(time.RFC3339),
		DetailsURL:  cr.DetailsURL,
		Output: &outputPayload{
			Title:   cr.Title,
			Summary: cr.Summary,
			Text:    cr.Text,
		},
	}
	if len(batches) > 0 {
		body.Output.Annotations = batches[0]
	}

	var req request
	if id == 0 {
		body.Name = cr.Name
		body.HeadSHA = cr.HeadSHA
		path, err := repoPath(owner, repo, "check-runs")
		if err != nil {
			return 0, err
		}
		req = request{method: http.MethodPost, path: path}
	} else {
		path, err := repoPath(owner, repo, "check-runs", fmt.Sprint(id))
		if err != nil {
			return 0, err
		}
		req = request{method: http.MethodPatch, path: path}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("encode check run %s for %s/%s@%s: %w", cr.Name, owner, repo, cr.HeadSHA, err)
	}
	req.body = raw

	var written struct {
		ID int64 `json:"id"`
	}
	if _, err := c.do(ctx, req, &written); err != nil {
		return 0, fmt.Errorf("write check run %s on %s/%s@%s: %w", cr.Name, owner, repo, cr.HeadSHA, err)
	}
	if written.ID == 0 {
		return 0, fmt.Errorf("write check run %s on %s/%s@%s: response carried no id", cr.Name, owner, repo, cr.HeadSHA)
	}

	// The remaining annotations go as updates to the check run just written.
	// GitHub appends them, so this is the only way past the fifty-per-request
	// cap — and also why a re-run at the same sha accumulates annotations: the
	// API has no way to clear them. Talooner only annotates when its own ruleset
	// load failed, so that costs a repeat on a PR that is already broken.
	for _, batch := range batches[min(1, len(batches)):] {
		if err := c.appendAnnotations(ctx, owner, repo, written.ID, cr, batch); err != nil {
			return written.ID, err
		}
	}
	return written.ID, nil
}

// appendAnnotations adds one more batch to an existing check run.
func (c *Client) appendAnnotations(ctx context.Context, owner, repo string, id int64, cr CheckRun, batch []annotationPayload) error {
	path, err := repoPath(owner, repo, "check-runs", fmt.Sprint(id))
	if err != nil {
		return err
	}
	raw, err := json.Marshal(checkRunPayload{
		Status:      "completed",
		Conclusion:  cr.Conclusion,
		CompletedAt: c.now().UTC().Format(time.RFC3339),
		Output: &outputPayload{
			Title:       cr.Title,
			Summary:     cr.Summary,
			Annotations: batch,
		},
	})
	if err != nil {
		return fmt.Errorf("encode annotations for check run %d on %s/%s: %w", id, owner, repo, err)
	}
	if _, err := c.do(ctx, request{method: http.MethodPatch, path: path, body: raw}, nil); err != nil {
		return fmt.Errorf("add annotations to check run %d on %s/%s: %w", id, owner, repo, err)
	}
	return nil
}

// findCheckRun returns the id of the check run of that name at sha, or 0 when
// there is none. Two ids for one name means an earlier run wrote a duplicate;
// the newest wins, so the check the PR shows is the one being updated.
func (c *Client) findCheckRun(ctx context.Context, owner, repo, name, sha string) (int64, error) {
	path, err := repoPath(owner, repo, "commits", sha, "check-runs")
	if err != nil {
		return 0, err
	}
	var payload struct {
		CheckRuns []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"check_runs"`
	}
	req := request{
		method: http.MethodGet,
		path:   path,
		query:  url.Values{"check_name": {name}, "per_page": {"100"}},
	}
	if _, err := c.do(ctx, req, &payload); err != nil {
		return 0, fmt.Errorf("look for check run %s on %s/%s@%s: %w", name, owner, repo, sha, err)
	}

	var id int64
	for _, run := range payload.CheckRuns {
		// check_name filters server-side, but a filter that quietly stopped
		// working would make every run create a check run instead of updating
		// one, which is exactly the duplication this call exists to prevent.
		if run.Name == name && run.ID > id {
			id = run.ID
		}
	}
	return id, nil
}

// batchAnnotations splits annotations into request-sized groups, dropping
// anything past the batch cap.
func batchAnnotations(as []Annotation) [][]annotationPayload {
	var batches [][]annotationPayload
	for i := 0; i < len(as) && len(batches) < maxAnnotationBatches; i += maxAnnotations {
		batch := as[i:min(i+maxAnnotations, len(as))]
		out := make([]annotationPayload, 0, len(batch))
		for _, a := range batch {
			out = append(out, annotationPayload{
				Path:            a.Path,
				StartLine:       a.StartLine,
				EndLine:         a.EndLine,
				AnnotationLevel: a.Level,
				Title:           a.Title,
				Message:         a.Message,
			})
		}
		batches = append(batches, out)
	}
	return batches
}
