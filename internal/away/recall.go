package away

// Recall: when a task starts, the local model is shown how similar tasks in
// the same project went before (examples.go keeps them). Up to two accepted
// ones (the task, the files, the diff you merged, the reviewer's note) and
// one lesson: what a reviewer sent back about the local model's attempt at
// a similar task (its "revise"/"fail" notes), whatever happened after. Similar = the task texts' embeddings (Ollama, nomic-embed-text) are
// close. Each task records which examples it was shown, so whether this
// helps can be measured. Nothing here can stop a task: without embeddings or
// matches, the model just gets the task.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Embedder turns texts into vectors (Ollama's /api/embed in production).
type Embedder func(ctx context.Context, texts []string) ([][]float64, error)

// Thresholds measured with nomic-embed-text and the "classification:" prefix
// against a real example (tests for a Kai deploy check), 10-17-26:
//
//	0.81  tests for another deploy check     (should be shown)
//	0.75  tests for a different helper       (useful: same conventions)
//	0.72  a feature in the same area (Kai)   (a test diff won't help much)
//	0.59  a different feature
//	0.55  an unrelated CSS fix
//
// The other prefixes separate these worse; topic dominates over the kind of
// task, so "same area, different work" sits close to the line.
const (
	recallGood   = 2    // accepted examples shown at most
	minGood      = 0.74 // cosine similarity needed for an accepted example
	minLesson    = 0.76 // and for a lesson
	maxShownDiff = 5000 // characters of each example's diff in the prompt
)

// OllamaEmbedder uses Ollama's /api/embed.
func OllamaEmbedder(host, model string) Embedder {
	client := &http.Client{Timeout: 60 * time.Second}
	base := strings.TrimRight(host, "/")
	return func(ctx context.Context, texts []string) ([][]float64, error) {
		body, _ := json.Marshal(map[string]any{"model": model, "input": texts})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/embed", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var out struct {
			Embeddings [][]float64 `json:"embeddings"`
			Error      string      `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, err
		}
		if out.Error != "" || len(out.Embeddings) != len(texts) {
			return nil, fmt.Errorf("embed: %s (got %d of %d)", out.Error, len(out.Embeddings), len(texts))
		}
		return out.Embeddings, nil
	}
}

// What the embedding model is asked to compare (nomic-embed-text wants a task prefix).
func embedText(task string) string { return "classification: " + task }

type embCache struct {
	Model   string               `json:"model"`
	Vectors map[string][]float64 `json:"vectors"` // example name -> vector of its task
}

func (s *Service) embedCachePath(project string) string {
	return filepath.Join(s.set.ExamplesDir, safeName(project), ".embeddings.json")
}

type scored struct {
	ex     Example
	name   string
	score  float64
	lesson string
}

// lessonOf: what reviewers sent back about the local model's attempts.
func lessonOf(ex Example) string {
	var notes []string
	for _, r := range ex.Rounds {
		if r.Who == "review" && (r.Verdict == "revise" || r.Verdict == "fail") && strings.TrimSpace(r.Notes) != "" {
			notes = append(notes, strings.TrimSpace(r.Notes))
		}
	}
	return strings.Join(notes, "\n")
}

// recall picks the examples to show for a new task; names says which.
func (s *Service) recall(ctx context.Context, t Task) (prompt string, names []string) {
	if s.embed == nil || s.set.ExamplesDir == "" {
		return "", nil
	}
	list, err := s.Examples(t.Project)
	if err != nil || len(list) == 0 {
		return "", nil
	}
	// Only finished ones with a decision say anything.
	var cands []ExampleSummary
	for _, e := range list {
		if e.Outcome == "accepted" || e.Outcome == "discarded" || e.Outcome == "retried" {
			cands = append(cands, e)
		}
	}
	if len(cands) == 0 {
		return "", nil
	}

	cache := embCache{Model: s.set.EmbedModel, Vectors: map[string][]float64{}}
	if b, err := os.ReadFile(s.embedCachePath(t.Project)); err == nil {
		var c embCache
		if json.Unmarshal(b, &c) == nil && c.Model == s.set.EmbedModel && c.Vectors != nil {
			cache = c
		}
	}
	var missing []ExampleSummary
	for _, e := range cands {
		if _, ok := cache.Vectors[e.Name]; !ok {
			missing = append(missing, e)
		}
	}
	texts := []string{embedText(t.Text)}
	for _, e := range missing {
		texts = append(texts, embedText(e.Task))
	}
	ectx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	vecs, err := s.embed(ectx, texts)
	if err != nil {
		s.logf("away: recall: %v", err)
		return "", nil
	}
	for i, e := range missing {
		cache.Vectors[e.Name] = vecs[i+1]
	}
	if len(missing) > 0 {
		if b, err := json.Marshal(cache); err == nil {
			_ = os.WriteFile(s.embedCachePath(t.Project), b, 0o644)
		}
	}

	var good, lessons []scored
	for _, e := range cands {
		sc := cosine(vecs[0], cache.Vectors[e.Name])
		b, err := s.ExampleJSON(e.Name)
		if err != nil {
			continue
		}
		var ex Example
		if json.Unmarshal(b, &ex) != nil {
			continue
		}
		if e.Outcome == "accepted" && sc >= minGood && strings.TrimSpace(ex.FinalDiff) != "" {
			good = append(good, scored{ex: ex, name: e.Name, score: sc})
		}
		if l := lessonOf(ex); l != "" && sc >= minLesson {
			lessons = append(lessons, scored{ex: ex, name: e.Name, score: sc, lesson: l})
		}
	}
	// Best first; the local model's own accepted work before Claude-fixed work at equal footing.
	sort.SliceStable(good, func(i, j int) bool {
		if (good[i].ex.Author == "qwen") != (good[j].ex.Author == "qwen") && math.Abs(good[i].score-good[j].score) < 0.05 {
			return good[i].ex.Author == "qwen"
		}
		return good[i].score > good[j].score
	})
	sort.SliceStable(lessons, func(i, j int) bool { return lessons[i].score > lessons[j].score })
	if len(good) > recallGood {
		good = good[:recallGood]
	}
	// A lesson from an example already shown goes with it, not twice.
	var lesson *scored
	for i := range lessons {
		if !containsName(good, lessons[i].name) {
			lesson = &lessons[i]
			break
		}
	}
	if len(good) == 0 && lesson == nil {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("\n\n---\n\n## How similar tasks went before in this project\n\n")
	if len(good) > 0 {
		b.WriteString("These were done and accepted. Use them as a guide to this project's conventions (where files go, how tests set things up, naming); the task above is what to do, so don't copy them blindly.\n")
		for _, g := range good {
			fmt.Fprintf(&b, "\n### Accepted: %s\n\nFiles: %s\n", firstLine(g.ex.Task, 300), strings.Join(g.ex.Files, ", "))
			if g.ex.Notes != "" {
				fmt.Fprintf(&b, "Reviewer's note: %s\n", firstLine(g.ex.Notes, 600))
			}
			if l := lessonOf(g.ex); l != "" {
				fmt.Fprintf(&b, "Its first attempt was sent back because: %s\n", clipText(l, 1200))
			}
			fmt.Fprintf(&b, "\n```diff\n%s```\n", strings.TrimRight(clipText(g.ex.FinalDiff, maxShownDiff), "\n")+"\n")
			names = append(names, g.name)
		}
	}
	if lesson != nil {
		fmt.Fprintf(&b, "\n### A similar task where the first attempt was sent back: %s\n\nThe reviewer's notes on it: %s\n", firstLine(lesson.ex.Task, 300), clipText(lesson.lesson, 1200))
		names = append(names, lesson.name)
	}
	return b.String(), names
}

// heldOut: whether this task runs without examples, for comparison. By id,
// so it's the same answer on every attempt (a retry can't switch sides).
func heldOut(id string, every int) bool {
	if every <= 0 {
		return false
	}
	var h uint32 = 2166136261
	for i := 0; i < len(id); i++ {
		h = (h ^ uint32(id[i])) * 16777619
	}
	return h%uint32(every) == 0
}

func containsName(list []scored, name string) bool {
	for _, x := range list {
		if x.name == name {
			return true
		}
	}
	return false
}

func cosine(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
