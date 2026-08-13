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
	signalText := failureSignalText(failure, message)
	if code, ok := failure.CodexErrorInfo.(string); ok {
		if transientStringCodes[code] {
			return classifiedFailure{Disposition: "transient", Code: code, Message: message}
		}
		// "other" is a generic fallback. Preserve explicit permanent message
		// signals, but allow clear transport/timeout text to refine it.
		if code == "other" && !permanentMessage.MatchString(signalText) && transientMessage.MatchString(signalText) {
			return classifiedFailure{Disposition: "transient", Code: "messageMatch", Message: message}
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
			if code == "other" && !permanentMessage.MatchString(signalText) && transientMessage.MatchString(signalText) {
				return classifiedFailure{Disposition: "transient", Code: "messageMatch", Message: message}
			}
			transient := transientObjectCodes[code] && isTransientTransportStatus(status)
			disposition := "permanent"
			if transient {
				disposition = "transient"
			}
			return classifiedFailure{Disposition: disposition, Code: code, HTTPStatus: status, Message: message}
		}
	}
	if permanentMessage.MatchString(signalText) {
		return classifiedFailure{Disposition: "permanent", Code: "messageMatch", Message: message}
	}
	if transientMessage.MatchString(signalText) {
		return classifiedFailure{Disposition: "transient", Code: "messageMatch", Message: message}
	}
	return classifiedFailure{Disposition: "permanent", Code: "unclassified", Message: message}
}

func classifyFailureAfterRetries(failure turnError, hadRetryableError bool) classifiedFailure {
	classified := classifyFailure(failure)
	if !hadRetryableError || classified.Disposition == "transient" || hasExplicitPermanentSignal(failure) {
		return classified
	}
	return classifiedFailure{Disposition: "transient", Code: "retryExhausted", HTTPStatus: classified.HTTPStatus, Message: classified.Message}
}

func failureSignalText(failure turnError, message string) string {
	if failure.AdditionalDetails == nil || *failure.AdditionalDetails == "" {
		return message
	}
	return message + " " + *failure.AdditionalDetails
}

func isTransientTransportStatus(status int) bool {
	return status == 0 || status >= 200 && status < 300 || status == 408 || status == 425 || status == 429 || status >= 500
}

func hasExplicitPermanentSignal(failure turnError) bool {
	message := failure.Message
	if message == "" {
		message = "Codex turn failed"
	}
	if permanentMessage.MatchString(failureSignalText(failure, message)) {
		return true
	}
	if code, ok := failure.CodexErrorInfo.(string); ok {
		return code != "other" && permanentStringCodes[code]
	}
	if info, ok := asObject(failure.CodexErrorInfo); ok {
		for code, value := range info {
			if code != "other" && permanentStringCodes[code] {
				return true
			}
			if !transientObjectCodes[code] {
				continue
			}
			if details, ok := asObject(value); ok {
				if number, ok := readNumber(details["httpStatusCode"]); ok {
					status := int(number)
					return status >= 400 && status < 500 && status != 408 && status != 425 && status != 429
				}
			}
		}
	}
	return false
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
