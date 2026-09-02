package clientprotocol

// outcomeSelected and outcomeCancelled are the two literal values the
// pinned schema declares for RequestPermissionOutcome's discriminator.
const (
	outcomeSelected  = requestPermissionOutcomeSelected
	outcomeCancelled = requestPermissionOutcomeCancelled
)

// selectRefusingOption scans options for a refusing kind, reject_once
// first and then reject_always, in the order the agent sent them, and
// ignores the option's identifier and name entirely. It reports
// found=false for an empty list or a list offering only allowing kinds;
// it never selects allow_once or allow_always under any condition.
func selectRefusingOption(options []permissionOption) (optionID string, found bool) {
	for _, opt := range options {
		if opt.Kind == permissionOptionKindRejectOnce {
			return string(opt.OptionID), true
		}
	}
	for _, opt := range options {
		if opt.Kind == permissionOptionKindRejectAlways {
			return string(opt.OptionID), true
		}
	}
	return "", false
}
