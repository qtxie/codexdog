package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func liveSupervisorState(store *stateStore) (supervisorState, error) {
	state, ok := store.Read()
	if !ok {
		return supervisorState{}, errors.New("no supervisor state exists for this workspace")
	}
	live, ok := queryControlState(state)
	if !ok {
		return supervisorState{}, errors.New("the Codexdog supervisor is not running for this workspace")
	}
	return live, nil
}

func runAgents(options supervisorOptions, store *stateStore) (int, error) {
	state, err := liveSupervisorState(store)
	if err != nil {
		return 1, err
	}
	if state.ProxyPort == 0 {
		return 1, errors.New("the supervisor has no App Server proxy endpoint")
	}
	args := make([]string, 0, len(options.CodexConfig)*2+5+len(options.TUIArgs))
	args = append(args, "agents")
	for _, value := range options.CodexConfig {
		args = append(args, "-c", value)
	}
	args = append(args, "--remote", fmt.Sprintf("ws://127.0.0.1:%d", state.ProxyPort))
	cwd := state.EffectiveCWD
	if cwd == "" {
		cwd = state.CWD
	}
	args = append(args, "-C", cwd)
	args = append(args, options.TUIArgs...)
	command := exec.Command(options.CodexPath, args...)
	command.Dir = options.CWD
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	err = command.Run()
	if err == nil {
		return 0, nil
	}
	return processExitCode(err), err
}

func runQueueCommand(args parsedArguments, store *stateStore) (int, error) {
	if len(args.CommandArgs) == 0 {
		return 1, errors.New("queue requires an action; use list, add, delete, update, reorder, or start")
	}
	state, err := liveSupervisorState(store)
	if err != nil {
		return 1, err
	}
	message, err := requestControlCommand(state, remoteCommand{Name: "queue", Text: strings.Join(args.CommandArgs, " ")})
	if err != nil {
		return 1, err
	}
	if args.JSON {
		data, err := json.MarshalIndent(map[string]any{"ok": true, "message": message}, "", "  ")
		if err != nil {
			return 1, err
		}
		fmt.Printf("%s\n", data)
		return 0, nil
	}
	fmt.Println(message)
	return 0, nil
}
