## 20. Webhook Support (Future Extension)

Sortie currently uses polling as the sole mechanism for tracker event delivery. This section
documents a planned extension point so the polling layer is designed to coexist with push-based
event delivery.

A future webhook receiver would accept HTTP POST events from the configured tracker (e.g., Jira
webhooks), parse them into the normalized issue model, and deliver them to the orchestrator as
immediate state updates. This would reduce polling latency for state changes without replacing the
polling loop entirely (polling remains the fallback and the source of truth for reconciliation).

Design constraints for coexistence:

- The polling loop must remain correct in the absence of webhooks.
- Webhook events should be treated as advisory triggers for an immediate reconciliation cycle, not
  as authoritative state transitions.
- Duplicate delivery (webhook + next poll cycle seeing the same state) must be idempotent.

This extension is not implemented in v1. Tracker adapters should not assume webhook availability.

