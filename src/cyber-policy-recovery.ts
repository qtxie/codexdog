export interface CyberPolicyRecoveryAction {
  kind: "retry-thread" | "fork-thread";
  prompt: string;
}

const ACTIONS: readonly CyberPolicyRecoveryAction[] = [
  { kind: "retry-thread", prompt: "continue" },
  { kind: "retry-thread", prompt: "继续" },
  { kind: "fork-thread", prompt: "continue" },
];

export function nextCyberPolicyRecoveryAction(
  attemptsSubmitted: number,
): CyberPolicyRecoveryAction | undefined {
  return ACTIONS[attemptsSubmitted];
}
