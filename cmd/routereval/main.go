// Command routereval replays labelled router calls against one or more
// models on an OpenAI-compatible endpoint and reports, per model, how often
// each labelled field was right, latency, token usage and estimated cost.
//
// The eval set is JSONL. Each line is a router.CallRecord (one line of
// router.log_file) plus an "expect" object with the right answer:
//
//	{"id": "group-question", ...call record..., "expect": {
//	   "addressed_to": "all",           // "all" | "user" | "none" | ["Name", ...]
//	   "user_question_pending": false,
//	   "plan": ["Ada", "Cleo"],         // the set of names, order ignored; [] = nobody
//	   "first": "Cleo",                 // plan[0]
//	   "note": "why"}}
//
// Every expect field is optional; only the ones given are scored. Lines
// without "expect" are skipped, so a copy of the call log can be labelled
// in place. Each case's system and input are sent as they were logged: the
// eval scores the model's raw answer, before roomd's deterministic checks.
//
// Usage:
//
//	routereval -models deepseek-v4-1-flash-1017-uncensored,z-ai-glm-5-3-flash \
//	  -price deepseek-v4-1-flash-1017-uncensored=0.33/1.32 -price z-ai-glm-5-3-flash=0.15/0.50 \
//	  evals/*.jsonl
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/ziliding/hermes-roomd/internal/router"
)

// Expect is the labelled right answer for one case.
type Expect struct {
	AddressedTo         json.RawMessage `json:"addressed_to,omitempty"`
	UserQuestionPending *bool           `json:"user_question_pending,omitempty"`
	Plan                *[]string       `json:"plan,omitempty"`
	First               string          `json:"first,omitempty"`
	Note                string          `json:"note,omitempty"`
}

// Case is one eval line: a logged router call and its label.
type Case struct {
	router.CallRecord
	ID     string  `json:"id,omitempty"`
	Expect *Expect `json:"expect,omitempty"`

	src string // file:line
}

// Name is the case's id, or where it came from.
func (c Case) Name() string {
	if c.ID != "" {
		return c.ID
	}
	return c.src
}

// The scored fields, in report order.
const (
	fParse   = "parsed"
	fAddr    = "addressed_to"
	fPending = "user_question_pending"
	fPlan    = "plan"
	fFirst   = "first"
	fAll     = "all_correct"
)

var fields = []string{fParse, fAddr, fPending, fPlan, fFirst, fAll}

// Result is one replayed call.
type Result struct {
	Model     string          `json:"model"`
	Case      string          `json:"case"`
	Output    string          `json:"output,omitempty"`
	Error     string          `json:"error,omitempty"`
	LatencyMS int64           `json:"latency_ms"`
	Usage     *router.Usage   `json:"usage,omitempty"`
	Reasoning int             `json:"reasoning_chars,omitempty"` // reasoning text returned beside the answer
	Scores    map[string]bool `json:"scores"`                    // field -> right (labelled fields only)
	Wrong     []string        `json:"wrong,omitempty"`           // "field: got X, want Y"
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Getenv, http.DefaultClient); err != nil {
		fmt.Fprintln(os.Stderr, "routereval:", err)
		os.Exit(1)
	}
}

type prices map[string][2]float64 // model -> USD per 1M input, output tokens

func (p prices) String() string { return fmt.Sprint(map[string][2]float64(p)) }

func (p prices) Set(v string) error {
	model, rest, ok := strings.Cut(v, "=")
	in, out, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || model == "" {
		return errors.New("want model=IN/OUT (USD per 1M tokens)")
	}
	a, err1 := strconv.ParseFloat(in, 64)
	b, err2 := strconv.ParseFloat(out, 64)
	if err1 != nil || err2 != nil || a < 0 || b < 0 {
		return errors.New("prices must be non-negative numbers")
	}
	p[model] = [2]float64{a, b}
	return nil
}

type config struct {
	baseURL, apiKey, effort string
	models                  []string
	prices                  prices
	jsonMode, verbose       bool
	repeat, concurrency     int
	timeout                 time.Duration
	out                     string
}

func run(ctx context.Context, args []string, stdout io.Writer, getenv func(string) string, hc *http.Client) error {
	fs := flag.NewFlagSet("routereval", flag.ContinueOnError)
	fs.SetOutput(stdout)
	c := config{prices: prices{}}
	var models, keyEnv string
	fs.StringVar(&c.baseURL, "base-url", "https://api.venice.ai/api/v1", "OpenAI-compatible base URL, including /v1")
	fs.StringVar(&keyEnv, "key-env", "ROOMD_ROUTER_API_KEY", "environment variable holding the API key")
	fs.StringVar(&models, "models", "", "comma-separated model ids to compare (default: each case's logged model)")
	fs.Var(c.prices, "price", "model=IN/OUT: USD per 1M input and output tokens, for the cost estimate (repeatable)")
	fs.BoolVar(&c.jsonMode, "json-mode", true, `send response_format {"type":"json_object"} (as router.json_mode)`)
	fs.StringVar(&c.effort, "reasoning-effort", "", "send reasoning_effort (e.g. none, low); empty: the model's default")
	fs.IntVar(&c.repeat, "n", 1, "replay each case this many times per model")
	fs.IntVar(&c.concurrency, "concurrency", 4, "calls in flight per model")
	fs.DurationVar(&c.timeout, "timeout", 60*time.Second, "per call")
	fs.StringVar(&c.out, "out", "", "write every call's result as JSONL to this file")
	fs.BoolVar(&c.verbose, "v", false, "list every wrong answer")
	fs.Usage = func() {
		fmt.Fprintln(stdout, "usage: routereval [flags] eval.jsonl...")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("no eval files")
	}
	c.apiKey = getenv(keyEnv)
	if c.apiKey == "" {
		return fmt.Errorf("environment variable %s is not set", keyEnv)
	}
	if c.repeat < 1 || c.concurrency < 1 {
		return errors.New("-n and -concurrency must be >= 1")
	}
	for _, m := range strings.Split(models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			c.models = append(c.models, m)
		}
	}

	var cases []Case
	skipped := 0
	for _, path := range fs.Args() {
		cs, n, err := readCases(path)
		if err != nil {
			return err
		}
		cases = append(cases, cs...)
		skipped += n
	}
	if len(cases) == 0 {
		return fmt.Errorf("no labelled cases (lines with \"expect\") in %s", strings.Join(fs.Args(), ", "))
	}
	if len(c.models) == 0 {
		for _, cs := range cases {
			if cs.Model != "" && !slices.Contains(c.models, cs.Model) {
				c.models = append(c.models, cs.Model)
			}
		}
	}
	if len(c.models) == 0 {
		return errors.New("no -models given and no model in the cases")
	}
	fmt.Fprintf(stdout, "%d labelled case(s) (%d unlabelled line(s) skipped), %d model(s), %d run(s) each\n\n",
		len(cases), skipped, len(c.models), c.repeat)

	var all []Result
	for _, m := range c.models {
		all = append(all, replay(ctx, c, hc, m, cases)...)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	report(stdout, c, all)
	if c.out != "" {
		var b bytes.Buffer
		for _, r := range all {
			line, _ := json.Marshal(r)
			b.Write(append(line, '\n'))
		}
		if err := os.WriteFile(c.out, b.Bytes(), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// readCases reads the labelled cases in one JSONL file and counts the
// lines without a label.
func readCases(path string) ([]Case, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	var out []Case
	skipped := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		var c Case
		if err := json.Unmarshal(line, &c); err != nil {
			return nil, 0, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if c.Expect == nil {
			skipped++
			continue
		}
		if c.System == "" || c.Input == "" {
			return nil, 0, fmt.Errorf("%s:%d: a case needs the logged system and input", path, n)
		}
		if _, err := wantAddressed(c.Expect.AddressedTo); err != nil {
			return nil, 0, fmt.Errorf("%s:%d: expect.addressed_to: %w", path, n, err)
		}
		c.src = fmt.Sprintf("%s:%d", path, n)
		out = append(out, c)
	}
	return out, skipped, sc.Err()
}

// replay runs every case c.repeat times against model.
func replay(ctx context.Context, c config, hc *http.Client, model string, cases []Case) []Result {
	results := make([]Result, len(cases)*c.repeat)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range c.concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = replayOne(ctx, c, hc, model, cases[i%len(cases)])
			}
		}()
	}
	for i := range results {
		select {
		case jobs <- i:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	return results
}

func replayOne(ctx context.Context, c config, hc *http.Client, model string, cs Case) Result {
	res := Result{Model: model, Case: cs.Name(), Scores: map[string]bool{}}
	start := time.Now()
	content, reasoning, usage, err := complete(ctx, c, hc, model, cs.System, cs.Input)
	res.LatencyMS = time.Since(start).Milliseconds()
	res.Output, res.Usage, res.Reasoning = content, usage, len(reasoning)
	if err == nil {
		var ans router.Answer
		if ans, err = router.ParseAnswer(content); err == nil {
			score(&res, *cs.Expect, ans)
			return res
		}
	}
	// No usable answer: every labelled field is wrong.
	res.Error = err.Error()
	score(&res, *cs.Expect, router.Answer{})
	for f := range res.Scores {
		res.Scores[f] = false
	}
	res.Wrong = nil
	return res
}

// score marks each labelled field right or wrong.
func score(res *Result, e Expect, a router.Answer) {
	res.Scores[fParse] = true
	mark := func(field string, ok bool, got, want any) {
		res.Scores[field] = ok
		if !ok {
			res.Wrong = append(res.Wrong, fmt.Sprintf("%s: got %v, want %v", field, got, want))
		}
	}
	if len(e.AddressedTo) > 0 {
		want, _ := wantAddressed(e.AddressedTo)
		got := a.Addressed()
		mark(fAddr, sameSet(got, want), got, want)
	}
	if e.UserQuestionPending != nil {
		mark(fPending, a.UserPending == *e.UserQuestionPending, a.UserPending, *e.UserQuestionPending)
	}
	if e.Plan != nil {
		mark(fPlan, sameSet(a.Plan, *e.Plan), a.Plan, *e.Plan)
	}
	if e.First != "" {
		got := ""
		if len(a.Plan) > 0 {
			got = a.Plan[0]
		}
		mark(fFirst, strings.EqualFold(got, e.First), got, e.First)
	}
	res.Scores[fAll] = len(res.Wrong) == 0
}

// wantAddressed reads expect.addressed_to: a keyword or name, or a list.
func wantAddressed(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, errors.New(`want "all", "user", "none", a name or a list of names`)
	}
	return list, nil
}

// sameSet compares two name lists as sets, ignoring case and order.
func sameSet(a, b []string) bool {
	norm := func(l []string) []string {
		var out []string
		for _, s := range l {
			s = strings.ToLower(strings.TrimSpace(s))
			if s == "everyone" {
				s = "all"
			}
			if s != "" && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(norm(a), norm(b))
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model           string            `json:"model"`
	Messages        []chatMessage     `json:"messages"`
	Temperature     float64           `json:"temperature"`
	Stream          bool              `json:"stream"`
	ResponseFormat  map[string]string `json:"response_format,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
		} `json:"message"`
	} `json:"choices"`
	Usage *router.Usage `json:"usage"`
}

// complete makes one chat-completions call the way the router does
// (temperature 0, no streaming).
func complete(ctx context.Context, c config, hc *http.Client, model, system, input string) (content, reasoning string, usage *router.Usage, err error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req := chatRequest{
		Model:           model,
		Messages:        []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: input}},
		ReasoningEffort: c.effort,
	}
	if c.jsonMode {
		req.ResponseFormat = map[string]string{"type": "json_object"}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", "", nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", "", nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := hc.Do(hreq)
	if err != nil {
		return "", "", nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", "", nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
		return "", "", nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", "", nil, fmt.Errorf("bad response JSON: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", "", cr.Usage, errors.New("no choices")
	}
	m := cr.Choices[0].Message
	return m.Content, m.ReasoningContent + m.Reasoning, cr.Usage, nil
}

// report prints one row per model: accuracy per field, then latency,
// tokens and cost.
func report(w io.Writer, c config, all []Result) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprint(tw, "model\t")
	for _, f := range fields {
		fmt.Fprint(tw, f+"\t")
	}
	fmt.Fprintln(tw)
	for _, m := range c.models {
		rs := byModel(all, m)
		fmt.Fprint(tw, m+"\t")
		for _, f := range fields {
			right, n := 0, 0
			for _, r := range rs {
				if ok, scored := r.Scores[f]; scored {
					n++
					if ok {
						right++
					}
				}
			}
			if n == 0 {
				fmt.Fprint(tw, "-\t")
				continue
			}
			fmt.Fprintf(tw, "%d/%d %3.0f%%\t", right, n, 100*float64(right)/float64(n))
		}
		fmt.Fprintln(tw)
	}
	tw.Flush()
	fmt.Fprintln(w)

	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "model\terrors\tp50 ms\tp95 ms\tprompt tok/call\tcompletion tok/call\treasoning tok/call\treasoning chars/call\tUSD total\tUSD per 1k calls\t")
	for _, m := range c.models {
		rs := byModel(all, m)
		var lat []int64
		var prompt, completion, reasoning, chars, withUsage, errs int
		for _, r := range rs {
			lat = append(lat, r.LatencyMS)
			if r.Error != "" {
				errs++
			}
			chars += r.Reasoning
			if r.Usage != nil {
				withUsage++
				prompt += r.Usage.PromptTokens
				completion += r.Usage.CompletionTokens
				if r.Usage.Details != nil {
					reasoning += r.Usage.Details.ReasoningTokens
				}
			}
		}
		slices.Sort(lat)
		per := func(n, d int) string {
			if d == 0 {
				return "-"
			}
			return strconv.Itoa((n + d/2) / d)
		}
		cost, perK := "-", "-"
		if p, ok := c.prices[m]; ok && withUsage > 0 {
			usd := (float64(prompt)*p[0] + float64(completion)*p[1]) / 1e6
			cost = fmt.Sprintf("%.4f", usd)
			perK = fmt.Sprintf("%.3f", usd/float64(withUsage)*1000)
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t\n", m, errs, pct(lat, 50), pct(lat, 95),
			per(prompt, withUsage), per(completion, withUsage), per(reasoning, withUsage), per(chars, len(rs)), cost, perK)
	}
	tw.Flush()
	fmt.Fprintln(w, "\nCost counts completion tokens (reasoning included) at the output price and ignores cached-input discounts.")

	for _, m := range c.models {
		for _, r := range byModel(all, m) {
			if r.Error == "" && len(r.Wrong) == 0 {
				continue
			}
			if !c.verbose {
				fmt.Fprintln(w, "\nrun with -v to list the wrong answers")
				return
			}
			why := strings.Join(r.Wrong, "; ")
			if r.Error != "" {
				why = r.Error
			}
			fmt.Fprintf(w, "WRONG %s %s: %s\n", m, r.Case, why)
		}
	}
}

func byModel(all []Result, model string) []Result {
	var out []Result
	for _, r := range all {
		if r.Model == model {
			out = append(out, r)
		}
	}
	return out
}

// pct is the p-th percentile of sorted values (nearest rank).
func pct(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := (p*len(sorted)+99)/100 - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}
