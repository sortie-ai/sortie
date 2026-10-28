export interface ServedTextProps {
  text: string;
}

/** Renders a sentence the server wrote as text, never as markup; text between a pair of backticks becomes inline code. */
export function ServedText({ text }: ServedTextProps) {
  const segments = text.split("`");
  const last = segments.length - 1;

  return (
    <>
      {segments.map((segment, index) => {
        const isPaired = index % 2 === 1 && index < last;
        if (isPaired) {
          return segment === "" ? undefined : (
            <code
              key={index}
              className="rounded bg-surface-raised px-xxs font-mono text-[0.9em] whitespace-nowrap"
            >
              {segment}
            </code>
          );
        }
        const prefix = index === last && index % 2 === 1 ? "`" : "";
        return prefix + segment === "" ? undefined : prefix + segment;
      })}
    </>
  );
}
