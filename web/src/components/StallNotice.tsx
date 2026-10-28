import { ServedText } from "@/components/ServedText";
import { StatusMark } from "@/components/StatusMark";

export interface StallNoticeProps {
  text: string;
  /** Whether the session is quiet; a notice of a session that is not carries no mark. */
  isStalled: boolean;
}

export function StallNotice({ text, isStalled }: StallNoticeProps) {
  return (
    <p className="mb-lg rounded-xl border border-solid border-line-strong bg-surface px-md py-sm text-body-md text-ink-body">
      {isStalled ? (
        <StatusMark mark="stalled" label={<ServedText text={text} />} />
      ) : (
        <ServedText text={text} />
      )}
    </p>
  );
}
