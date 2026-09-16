package procutil

import "time"

func armGroupEscalation(pid int, grace time.Duration) {
	target, ok := captureGroupEscalation(pid)
	if !ok {
		return
	}

	go func() {
		defer target.close()

		deadline := time.Now().Add(grace)
		for time.Now().Before(deadline) {
			time.Sleep(min(groupDrainPollInterval, time.Until(deadline)))
			if present, err := target.hasRunningMember(); err == nil && !present {
				return
			}
		}

		if present, err := target.hasRunningMember(); err == nil && !present {
			return
		}
		_ = target.terminateAll()
	}()
}
