package usagesource

import (
	"encoding/json"

	"github.com/sortie-ai/sortie/internal/agent/usage"
)

const (
	// kiroCreditUnit is the plural display name Kiro's credits are recorded
	// and pulled under.
	kiroCreditUnit = "credits"

	// kiroMetadataMethod carries the default engine's credit frame.
	kiroMetadataMethod = "_kiro.dev/metadata"

	// kiroCreditEntryUnit is the unit an entry names for a credit; an entry of
	// any other unit is not read.
	kiroCreditEntryUnit = "credit"

	kiroMeteringAmountMember = "value"
	kiroSummaryAmountMember  = "usage"

	kiroSessionInfoUpdate = "session_info_update"
	kiroTurnCompletion    = "turn_completion"
)

// creditReadings reads one reading per credit entry of a JSON array, taking
// each amount from the entry's amountMember. A missing or malformed amount
// reads unknown, never zero.
func creditReadings(entries json.RawMessage, amountMember string) []usage.AmountReading {
	var items []json.RawMessage
	if err := json.Unmarshal(entries, &items); err != nil {
		return nil
	}
	var readings []usage.AmountReading
	for _, item := range items {
		entry, ok := jsonObject(item)
		if !ok {
			continue
		}
		if unit, _ := jsonString(entry["unit"]); unit != kiroCreditEntryUnit {
			continue
		}
		readings = append(readings, usage.AmountReading{Amount: jsonAmount(entry[amountMember])})
	}
	return readings
}

// kiroSessionUpdateAmounts reads the credits a turn_completion session info
// update of the other engine reports.
func kiroSessionUpdateAmounts(update json.RawMessage) []usage.AmountReading {
	if kind, _ := jsonString(jsonMember(update, "sessionUpdate")); kind != kiroSessionInfoUpdate {
		return nil
	}
	meta := jsonMember(jsonMember(update, "_meta"), "kiro")
	if kind, _ := jsonString(jsonMember(meta, "kind")); kind != kiroTurnCompletion {
		return nil
	}
	return creditReadings(jsonMember(meta, "promptTurnSummaries"), kiroSummaryAmountMember)
}
