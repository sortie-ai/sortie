import { Panel } from "@/components/Panel";
import { ServedText } from "@/components/ServedText";
import { StatusMark } from "@/components/StatusMark";

export interface StallNoticeProps {
  text: string;
  /** Whether the session is quiet; a notice of a session that is not carries no mark. */
  isStalled: boolean;
}

/** The notice for a stalled run, set as a section of its screen. */
export function StallNotice({ text, isStalled }: StallNoticeProps) {
  return (
    <div className="mb-xl">
      <Panel title={isStalled ? "Stalled" : "Stall detection"}>
        <p className="rounded-xl border border-line bg-canvas px-md py-sm text-body-md text-ink-body">
          {isStalled ? (
            <StatusMark mark="stalled" label={<ServedText text={text} />} />
          ) : (
            <ServedText text={text} />
          )}
        </p>
      </Panel>
    </div>
  );
}
