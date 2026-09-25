package router

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/primaybr/liltok/internal/provider"
)

// PlanModeContext describes the Claude Code plan-mode state carried in a request's history.
type PlanModeContext struct {
	Active      bool
	PlanPath    string
	PlanWritten bool
}

var (
	// Claude Code plan-mode reminders name the plan file in one of these two forms.
	planPathCreateRegex = regexp.MustCompile(`create your plan at (.+?) using the Write tool`)
	planPathExistsRegex = regexp.MustCompile(`A plan file already exists at (.+?)\. You can`)
	systemReminderRegex = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
)

// extractPlanModeContext scans the request history for the latest plan-mode reminder,
// whether plan mode has since been exited, and whether the plan file was written.
func extractPlanModeContext(req *provider.UnifiedChatRequest) PlanModeContext {
	var ctx PlanModeContext
	if req == nil {
		return ctx
	}

	pathIdx := -1
	planReminder := ""
	for i, msg := range req.Messages {
		// Claude Code delivers plan-mode state as <system-reminder> blocks in user or system turns.
		// Tool output is excluded: a file or command output quoting the reminder text must not
		// switch plan mode on.
		if msg.Role != "user" && msg.Role != "system" {
			continue
		}
		reminders := strings.Join(systemReminderRegex.FindAllString(msg.Content, -1), "\n")
		if path := findPlanPath(reminders); path != "" {
			ctx.PlanPath = path
			pathIdx = i
			planReminder = reminders
		}
	}
	if pathIdx < 0 {
		return ctx
	}
	ctx.Active = true

	normPlan := normalizePlanPath(ctx.PlanPath)
	for _, msg := range req.Messages[pathIdx+1:] {
		switch msg.Role {
		case "assistant":
			for _, tc := range msg.ToolCalls {
				name := strings.ToLower(tc.Function.Name)
				if name != "write" && name != "edit" {
					continue
				}
				var args struct {
					FilePath string `json:"file_path"`
				}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err == nil &&
					normalizePlanPath(args.FilePath) == normPlan {
					ctx.PlanWritten = true
				}
			}
		case "tool":
			lower := strings.ToLower(msg.Content)
			if strings.Contains(lower, "approved exiting plan mode") || strings.Contains(lower, "exited plan mode") {
				ctx.Active = false
			}
		}
	}

	// Plan files written before the latest reminder also count (the "already exists" form).
	if planPathExistsRegex.MatchString(planReminder) {
		ctx.PlanWritten = true
	}
	return ctx
}

func findPlanPath(content string) string {
	var path string
	for _, re := range []*regexp.Regexp{planPathCreateRegex, planPathExistsRegex} {
		if all := re.FindAllStringSubmatch(content, -1); len(all) > 0 {
			path = strings.TrimSpace(all[len(all)-1][1])
		}
	}
	return path
}

// normalizePlanPath compares Windows and POSIX spellings of the same path.
func normalizePlanPath(p string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), `\`, "/"))
}

// resolvePlanText returns the plan the model produced for an ExitPlanMode call:
// explicit tool arguments first, then visible text, then a structured plan in the thinking.
// Unstructured thinking does not count as a plan.
func resolvePlanText(textContent, thinkingText, argsStr string) string {
	if plan, _ := extractPlanFromArguments(argsStr); plan != "" {
		return plan
	}
	if strings.TrimSpace(textContent) != "" {
		return strings.TrimSpace(textContent)
	}
	for _, re := range []*regexp.Regexp{planHeaderRegex, numberedStepRegex} {
		if m := re.FindStringSubmatch(thinkingText); len(m) > 1 {
			return strings.TrimSpace(trailingThoughtsRegex.ReplaceAllString(m[1], ""))
		}
	}
	return ""
}

// isEmptyExitPlanMode reports an ExitPlanMode call made in plan mode before the plan file
// was written and with no plan text to write into it.
func isEmptyExitPlanMode(req *provider.UnifiedChatRequest, resp *provider.UnifiedChatResponse) bool {
	ctx := extractPlanModeContext(req)
	if !ctx.Active || ctx.PlanWritten {
		return false
	}
	thinking, text := extractThinkingBlocks(resp.Content)
	if resp.ReasoningContent != "" {
		thinking = resp.ReasoningContent + "\n" + thinking
	}
	for _, tc := range resp.ToolCalls {
		if isExitPlanMode(tc.Function.Name) && resolvePlanText(text, thinking, tc.Function.Arguments) == "" {
			return true
		}
	}
	return false
}

// isAutomatedUserMessage reports user-role content injected by the client rather than typed by a human.
func isAutomatedUserMessage(content string) bool {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "[System Reminder]") {
		return true
	}
	if !strings.Contains(trimmed, "<system-reminder>") {
		return false
	}
	return strings.TrimSpace(systemReminderRegex.ReplaceAllString(trimmed, "")) == ""
}
