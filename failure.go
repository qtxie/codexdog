package main

import (
	"fmt"
	"regexp"
)

type classifiedFailure struct {
	Disposition string
	Code        string
	HTTPStatus  int
	Message     string
}

var transientStringCodes = map[string]bool{
	"serverOverloaded":    true,
	"internalServerError": true,
}

var permanentStringCodes = map[string]bool{
	"activeTurnNotSteerable": true,
	"badRequest":             true,
	"contextWindowExceeded":  true,
	"cyberPolicy":            true,
	"other":                  true,
	"sandboxError":           true,
	"sessionBudgetExceeded":  true,
	"threadRollbackFailed":   true,
	"unauthorized":           true,
	"usageLimitExceeded":     true,
}

var transientObjectCodes = map[string]bool{
	"httpConnectionFailed":           true,
	"responseStreamConnectionFailed": true,
	"responseStreamDisconnected":     true,
	"responseTooManyFailedAttempts":  true,
}

var permanentMessage = regexp.MustCompile(`(?i)\b(unauthori[sz]ed|forbidden|invalid api key|authentication|bad request|context window|usage limit|quota exhausted|sandbox)\b`)
var transientMessage = regexp.MustCompile(`(?i)\b(timed?\s*out|timeout|connection (closed|failed|refused|reset)|network error|dns|tls|socket|stream disconnected|temporarily unavailable|server overloaded|too many requests|http\s*(408|425|429|5[0-9][0-9]))\b`)

func classifyFailure(failure turnError) classifiedFailure {
	message := failure.Message
	if message == "" {
		message = "Codex turn failed"
	}
	if code, ok := failure.CodexErrorInfo.(string); ok {
		if transientStringCodes[code] {
			return classifiedFailure{Disposition: "transient", Code: code, Message: message}
		}
		if permanentStringCodes[code] {
			return classifiedFailure{Disposition: "permanent", Code: code, Message: message}
		}
	}
	if info, ok := asObject(failure.CodexErrorInfo); ok {
		for _, code := range []string{"httpConnectionFailed", "responseStreamConnectionFailed", "responseStreamDisconnected", "responseTooManyFailedAttempts", "activeTurnNotSteerable", "badRequest", "contextWindowExceeded", "cyberPolicy", "other", "sandboxError", "sessionBudgetExceeded", "threadRollbackFailed", "unauthorized", "usageLimitExceeded"} {
			value, exists := info[code]
			if !exists {
				continue
			}
			status := 0
			if details, ok := asObject(value); ok {
				if number, ok := readNumber(details["httpStatusCode"]); ok {
					status = int(number)
				}
			}
			transient := transientObjectCodes[code] && (status == 0 || status == 408 || status == 425 || status == 429 || status >= 500)
			disposition := "permanent"
			if transient {
				disposition = "transient"
			}
			return classifiedFailure{Disposition: disposition, Code: code, HTTPStatus: status, Message: message}
		}
	}
	if permanentMessage.MatchString(message) {
		return classifiedFailure{Disposition: "permanent", Code: "messageMatch", Message: message}
	}
	if transientMessage.MatchString(message) {
		return classifiedFailure{Disposition: "transient", Code: "messageMatch", Message: message}
	}
	return classifiedFailure{Disposition: "permanent", Code: "unclassified", Message: message}
}

func formatFailure(failure classifiedFailure) string {
	status := ""
	if failure.HTTPStatus != 0 {
		status = fmt.Sprintf(" HTTP %d", failure.HTTPStatus)
	}
	return fmt.Sprintf("%s%s: %s", failure.Code, status, failure.Message)
}

type cyberPolicyAction struct {
	Kind   string
	Prompt string
}

var cyberPolicyActions = []cyberPolicyAction{
	{Kind: "retry-thread", Prompt: "continue"},
	{Kind: "retry-thread", Prompt: "继续"},
	{Kind: "fork-thread", Prompt: "continue"},
}

func nextCyberPolicyAction(attempts int) (cyberPolicyAction, bool) {
	if attempts < 0 || attempts >= len(cyberPolicyActions) {
		return cyberPolicyAction{}, false
	}
	return cyberPolicyActions[attempts], true
}
