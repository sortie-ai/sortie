import { createContext, use, useCallback, useEffect, useMemo, useRef, useState } from "react";

import type { ReactNode } from "react";

export interface ScreenCommand {
  /** Identifies the command in the palette; a screen keeps it while the command stays. */
  value: string;
  label: string;
  run: () => void;
}

interface ScreenCommandsValue {
  commands: readonly ScreenCommand[];
  publish: (commands: readonly ScreenCommand[]) => void;
}

const ScreenCommandsContext = createContext<ScreenCommandsValue>({
  commands: [],
  publish: () => {},
});

function sameValues(a: readonly ScreenCommand[], b: readonly ScreenCommand[]): boolean {
  return a.length === b.length && a.every((command, index) => command.value === b[index]?.value);
}

export interface ScreenCommandsProviderProps {
  children: ReactNode;
}

/** Holds the commands the mounted screen offers, for the palette beside it to list. */
export function ScreenCommandsProvider({ children }: ScreenCommandsProviderProps) {
  const [commands, setCommands] = useState<readonly ScreenCommand[]>([]);
  // The same values in the same order keep the state, so a screen cannot loop the provider by
  // listing its commands on every render.
  const publish = useCallback(
    (next: readonly ScreenCommand[]) =>
      setCommands((current) => (sameValues(current, next) ? current : next)),
    [],
  );
  const value = useMemo(() => ({ commands, publish }), [commands, publish]);

  return <ScreenCommandsContext value={value}>{children}</ScreenCommandsContext>;
}

/** Offers commands to the palette while the calling screen is mounted. Each `run` calls the closure of the screen's latest render. */
export function useScreenCommands(commands: readonly ScreenCommand[]): void {
  const { publish } = use(ScreenCommandsContext);
  const latest = useRef(commands);
  const signature = commands.map((command) => command.value).join("\n");

  useEffect(() => {
    latest.current = commands;
  });

  useEffect(() => {
    publish(
      signature === ""
        ? []
        : signature.split("\n").map((value) => ({
            value,
            label: latest.current.find((command) => command.value === value)?.label ?? value,
            run: () => latest.current.find((command) => command.value === value)?.run(),
          })),
    );
  }, [signature, publish]);

  useEffect(() => () => publish([]), [publish]);
}

/** The commands the mounted screen offers, in the order it listed them. */
export function useProvidedScreenCommands(): readonly ScreenCommand[] {
  return use(ScreenCommandsContext).commands;
}
