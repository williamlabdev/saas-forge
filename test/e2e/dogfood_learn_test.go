package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// Dogfood: cinqing learn's real course corpus against the real API.
//
// Gated on LEARN_CONTENT (a path to courses.generated.json) the same way the
// read-path harness is gated on MEASURE, and for the same reason — the fixture
// lives in another repository, so CI has nothing to point it at:
//
//	LEARN_CONTENT=~/dev/source/core/brand/cinqing/apps/learn/src/content/courses.generated.json \
//	  go test ./test/e2e/ -run TestDogfood_Learn -v
//
// What this is for: OD-007 asks work on the delivery path to name something
// that is broken NOW. Synthetic seeds cannot answer that — they are shaped by
// whoever writes them, so they only ever break where the author expected. Real
// content breaks where the MODEL is thin, which is the signal worth having.
//
// It therefore models learn the way learn is actually shaped and reports what
// the API refuses, rather than pre-solving the shape into something known to
// fit. A run that passes by having quietly flattened the awkward parts would
// answer the wrong question.
type learnCourse struct {
	Slug       string      `json:"slug"`
	Title      string      `json:"title"`
	Summary    string      `json:"summary"`
	Level      string      `json:"level"`
	Tier       string      `json:"tier"`
	Tags       []string    `json:"tags"`
	Order      int         `json:"order"`
	Rank       *string     `json:"rank"`
	Capstone   any         `json:"capstone"`
	ComingSoon bool        `json:"comingSoon"`
	Units      []learnUnit `json:"units"`
}

type learnUnit struct {
	Title   string        `json:"title"`
	Lessons []learnLesson `json:"lessons"`
}

type learnLesson struct {
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	Summary   string    `json:"summary"`
	Tier      string    `json:"tier"`
	Free      bool      `json:"free"`
	YouTubeID string    `json:"youtubeId"`
	Body      string    `json:"body"`
	Quiz      learnQuiz `json:"quiz"`
}

type learnQuiz struct {
	Pass      int             `json:"pass"`
	Questions []learnQuestion `json:"questions"`
}

type learnQuestion struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	Prompt  string   `json:"prompt"`
	Options []string `json:"options"`
	Answer  []int    `json:"answer"`
	Explain string   `json:"explain"`
	Power   int      `json:"power"`
}

// wall is one thing the content model could not express. Collected rather than
// fatal: stopping at the first refusal would report one wall per run, and the
// question being asked is how many there are.
type wall struct {
	where  string
	what   string
	code   string
	detail string
}

type wallLog struct {
	walls []wall
}

func (w *wallLog) add(where, what, code, detail string) {
	w.walls = append(w.walls, wall{where: where, what: what, code: code, detail: detail})
}

// codeFromBody pulls the API's error code out of the envelope so a wall is
// recorded by what the product called it, not by this test's paraphrase.
func codeFromBody(body string) (string, string) {
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details any    `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Error == nil {
		return "", body
	}
	d, _ := json.Marshal(env.Error.Details)
	return env.Error.Code, env.Error.Message + " " + string(d)
}

func TestDogfood_LearnCorpus(t *testing.T) {
	requireE2E(t)
	path := os.Getenv("LEARN_CONTENT")
	if path == "" {
		t.Skip("set LEARN_CONTENT to cinqing learn's courses.generated.json")
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var courses []learnCourse
	require.NoError(t, json.Unmarshal(raw, &courses))

	units, lessons, questions := 0, 0, 0
	for _, c := range courses {
		units += len(c.Units)
		for _, u := range c.Units {
			lessons += len(u.Lessons)
			for _, l := range u.Lessons {
				questions += len(l.Quiz.Questions)
			}
		}
	}
	t.Logf("corpus: %d courses / %d units / %d lessons / %d quiz questions",
		len(courses), units, lessons, questions)

	_, _, login := registerAndLogin(t, "learn")
	token := login["access_token"].(string)
	log := &wallLog{}

	post := func(path, body string) *testRec {
		rec := doJSON(t, http.MethodPost, path, body, "Bearer "+token, "", e2eClientIP(t))
		return &testRec{code: rec.Code, body: rec.Body.String()}
	}

	// --- schema ------------------------------------------------------------
	//
	// The natural mapping. `quiz` and `question` are separate types reached by
	// relation rather than nested objects, because that is what the model
	// supports today — the first question this run answers is whether that is
	// enough, and inventing a nested field type before trying it would skip the
	// step that produces the evidence.
	types := []struct {
		name   string
		fields string
	}{
		{"quiz_question", `[
			{"key":"qid","type":"string","label":"ID","required":true},
			{"key":"kind","type":"enum","label":"Kind","enum_values":["single","multiple","boolean","scenario"],"required":true},
			{"key":"prompt","type":"text","label":"Prompt","required":true},
			{"key":"options","type":"string","label":"Options","multiple":true},
			{"key":"answer","type":"number","label":"Answer","multiple":true},
			{"key":"explain","type":"text","label":"Explain"},
			{"key":"power","type":"number","label":"Power"}
		]`},
		{"quiz", `[
			{"key":"pass","type":"number","label":"Pass","required":true},
			{"key":"questions","type":"relation","relation_entity":"quiz_question","label":"Questions","multiple":true}
		]`},
		{"lesson", `[
			{"key":"slug","type":"string","label":"Slug","required":true},
			{"key":"title","type":"string","label":"Title","required":true},
			{"key":"summary","type":"text","label":"Summary"},
			{"key":"tier","type":"enum","label":"Tier","enum_values":["free","daily-magic","advanced","master"]},
			{"key":"free","type":"boolean","label":"Free"},
			{"key":"youtube_id","type":"string","label":"YouTube"},
			{"key":"body","type":"richtext","label":"Body"},
			{"key":"quiz","type":"relation","relation_entity":"quiz","label":"Quiz"}
		]`},
		{"unit", `[
			{"key":"title","type":"string","label":"Title","required":true},
			{"key":"lessons","type":"relation","relation_entity":"lesson","label":"Lessons","multiple":true}
		]`},
		{"course", `[
			{"key":"slug","type":"string","label":"Slug","required":true},
			{"key":"title","type":"string","label":"Title","required":true},
			{"key":"summary","type":"text","label":"Summary"},
			{"key":"level","type":"string","label":"Level"},
			{"key":"tier","type":"enum","label":"Tier","enum_values":["free","daily-magic","advanced","master"]},
			{"key":"tags","type":"string","label":"Tags","multiple":true},
			{"key":"sort_order","type":"number","label":"Order"},
			{"key":"rank","type":"string","label":"Rank"},
			{"key":"coming_soon","type":"boolean","label":"Coming soon"},
			{"key":"units","type":"relation","relation_entity":"unit","label":"Units","multiple":true}
		]`},
	}

	created := map[string]bool{}
	for _, ty := range types {
		rec := post("/api/v1/content/types",
			fmt.Sprintf(`{"name":%q,"label":%q,"fields":%s}`, ty.name, ty.name, ty.fields))
		if rec.code != http.StatusCreated {
			code, detail := codeFromBody(rec.body)
			log.add("type:"+ty.name, "whole type refused", code, detail)
			t.Logf("WALL type %-14s %s — %s", ty.name, code, detail)
			continue
		}
		created[ty.name] = true
	}

	// --- entries -----------------------------------------------------------
	//
	// Bottom-up, so a relation always names something already written.
	entry := func(typeName string, payload map[string]any) (string, bool) {
		b, err := json.Marshal(payload)
		require.NoError(t, err)
		rec := post("/api/v1/content/entries?type="+typeName, string(b))
		if rec.code != http.StatusCreated {
			code, detail := codeFromBody(rec.body)
			log.add("entry:"+typeName, fmt.Sprintf("%v", payload["slug"]), code, detail)
			return "", false
		}
		var env struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(rec.body), &env))
		return env.Data.ID, true
	}

	if !created["quiz_question"] || !created["quiz"] || !created["lesson"] ||
		!created["unit"] || !created["course"] {
		t.Log("schema incomplete — entry import skipped for the missing types")
	}

	imported := map[string]int{}
	for _, c := range courses {
		unitIDs := []any{}
		for ui, u := range c.Units {
			lessonIDs := []any{}
			for _, l := range u.Lessons {
				questionIDs := []any{}
				for _, q := range l.Quiz.Questions {
					answers := make([]any, len(q.Answer))
					for i, a := range q.Answer {
						answers[i] = a
					}
					opts := make([]any, len(q.Options))
					for i, o := range q.Options {
						opts[i] = o
					}
					if created["quiz_question"] {
						if id, ok := entry("quiz_question", map[string]any{
							"qid": q.ID, "kind": q.Type, "prompt": q.Prompt,
							"options": opts, "answer": answers,
							"explain": q.Explain, "power": q.Power,
						}); ok {
							questionIDs = append(questionIDs, id)
							imported["quiz_question"]++
						}
					}
				}
				quizID := ""
				if created["quiz"] {
					if id, ok := entry("quiz", map[string]any{
						"pass": l.Quiz.Pass, "questions": questionIDs,
					}); ok {
						quizID = id
						imported["quiz"]++
					}
				}
				if created["lesson"] {
					payload := map[string]any{
						"slug": l.Slug, "title": l.Title, "summary": l.Summary,
						"tier": l.Tier, "free": l.Free, "youtube_id": l.YouTubeID,
						"body": l.Body,
					}
					if quizID != "" {
						payload["quiz"] = quizID
					}
					if id, ok := entry("lesson", payload); ok {
						lessonIDs = append(lessonIDs, id)
						imported["lesson"]++
					}
				}
			}
			if created["unit"] {
				if id, ok := entry("unit", map[string]any{
					"title": u.Title, "lessons": lessonIDs,
				}); ok {
					unitIDs = append(unitIDs, id)
					imported["unit"]++
				}
			}
			_ = ui
		}
		if created["course"] {
			tags := make([]any, len(c.Tags))
			for i, tg := range c.Tags {
				tags[i] = tg
			}
			payload := map[string]any{
				"slug": c.Slug, "title": c.Title, "summary": c.Summary,
				"level": c.Level, "tier": c.Tier, "tags": tags,
				"sort_order": c.Order, "coming_soon": c.ComingSoon,
				"units": unitIDs,
			}
			if c.Rank != nil {
				payload["rank"] = *c.Rank
			}
			if _, ok := entry("course", payload); ok {
				imported["course"]++
			}
		}
	}

	// --- report ------------------------------------------------------------
	t.Log("imported:")
	for _, k := range []string{"course", "unit", "lesson", "quiz", "quiz_question"} {
		t.Logf("   %-14s %d", k, imported[k])
	}

	byCode := map[string][]wall{}
	for _, w := range log.walls {
		byCode[w.code+" @ "+w.where] = append(byCode[w.code+" @ "+w.where], w)
	}
	keys := make([]string, 0, len(byCode))
	for k := range byCode {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("walls: %d refusals in %d distinct kinds", len(log.walls), len(keys))
	for _, k := range keys {
		ws := byCode[k]
		t.Logf("   %-46s x%-5d e.g. %s", k, len(ws), ws[0].detail)
	}
}

type testRec struct {
	code int
	body string
}
