import { ServedText } from "@/components/ServedText";
import { StatusMark } from "@/components/StatusMark";
import { runStatus } from "@/lib/run-status";

export interface RunStatusTextProps {
  status: string;
}

/** Words a run's persisted status with its mark; a run still going reads as running. */
export function RunStatusText({ status }: RunStatusTextProps) {
  if (status === "running") {
    return <StatusMark mark="running" label="Running" />;
  }
  const { word, mark } = runStatus(status);
  return mark === null ? (
    <ServedText text={word} />
  ) : (
    <StatusMark mark={mark} label={<ServedText text={word} />} />
  );
}
