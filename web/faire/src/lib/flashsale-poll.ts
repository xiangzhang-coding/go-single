export interface PollBudget {
  lifecycleID?: string;
  attempts: number;
}

export function resetPollBudget(budget: PollBudget, lifecycleID?: string): PollBudget {
  return budget.lifecycleID === lifecycleID ? budget : { lifecycleID, attempts: 0 };
}

export function recordPollAttempt(budget: PollBudget, lifecycleID: string): PollBudget {
  const current = resetPollBudget(budget, lifecycleID);
  return { ...current, attempts: current.attempts + 1 };
}
