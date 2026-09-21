package prewarm

import (
	"bytes"
	"encoding/json"
	"strings"
)

const maxEventBytes = 1 << 20
const MaxProbeBytes = 4 << 20

// Completion inspects bounded SSE events. A probe is successful only after an
// explicit successful completion with the exact requested model (not an alias).
// It never changes or buffers delivery of business response chunks.
type Completion struct {
	expected  string
	line      []byte
	event     []byte
	completed bool
	mismatch  bool
	actual    string
	malformed bool
}

func NewCompletion(model string) *Completion { return &Completion{expected: model} }
func (c *Completion) Feed(p []byte) {
	if c.malformed {
		return
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			i = len(p)
		}
		if len(c.line)+i > maxEventBytes {
			c.malformed = true
			c.line = nil
			c.event = nil
			return
		}
		c.line = append(c.line, p[:i]...)
		if i == len(p) {
			return
		}
		c.consumeLine()
		p = p[i+1:]
		if c.malformed {
			return
		}
	}
}
func (c *Completion) consumeLine() {
	line := bytes.TrimSuffix(c.line, []byte{'\r'})
	if len(line) == 0 {
		c.consumeEvent()
	} else if bytes.HasPrefix(line, []byte("data:")) {
		data := bytes.TrimPrefix(line[5:], []byte{' '})
		if len(c.event)+len(data)+1 > maxEventBytes {
			c.malformed = true
			c.event = nil
		} else {
			c.event = append(c.event, data...)
			c.event = append(c.event, '\n')
		}
	}
	c.line = nil
}
func (c *Completion) consumeEvent() {
	raw := bytes.TrimSpace(c.event)
	c.event = nil
	if len(raw) == 0 || bytes.Equal(raw, []byte("[DONE]")) {
		return
	}
	c.JSON(raw)
}
func (c *Completion) Finish() {
	if len(c.line) > 0 {
		c.consumeLine()
	}
	c.consumeEvent()
}
func (c *Completion) JSON(raw []byte) {
	if len(raw) > maxEventBytes {
		c.malformed = true
		return
	}
	var event struct {
		Type     string          `json:"type"`
		Object   string          `json:"object"`
		Status   string          `json:"status"`
		Model    string          `json:"model"`
		Error    json.RawMessage `json:"error"`
		Response *struct {
			Status string          `json:"status"`
			Model  string          `json:"model"`
			Error  json.RawMessage `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &event) != nil {
		c.malformed = true
		return
	}
	noError := func(e json.RawMessage) bool { return len(e) == 0 || string(bytes.TrimSpace(e)) == "null" }
	var model string
	if event.Type == "response.completed" && event.Response != nil && noError(event.Error) && noError(event.Response.Error) && (event.Response.Status == "completed" || event.Response.Status == "") {
		model = event.Response.Model
	} else if event.Object == "response" && event.Status == "completed" && noError(event.Error) {
		model = event.Model
	}
	if strings.TrimSpace(model) != "" {
		c.actual = model
		c.completed = true
		if model != c.expected {
			c.mismatch = true
		}
	}
}
func (c *Completion) ActualModel() string { return c.actual }
func (c *Completion) Success() bool       { return c.completed && !c.mismatch && !c.malformed }
func (c *Completion) Mismatch() bool      { return c.completed && c.mismatch }

func EvaluateProbeBody(body []byte, model string) (bool, string) {
	o := NewCompletion(model)
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		o.JSON(body)
	} else {
		o.Feed(body)
		o.Finish()
	}
	return o.Success(), o.ActualModel()
}
func SuccessfulProbe(result ProbeResult, model string) bool {
	if result.Status != 200 || len(result.Body) > MaxProbeBytes {
		return false
	}
	ok, _ := EvaluateProbeBody(result.Body, model)
	return ok
}
