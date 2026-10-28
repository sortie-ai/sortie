import { useSyncExternalStore } from "react";

import { MonitorIcon, MoonIcon, SunIcon } from "@/components/icons";
import { buttonStyles } from "@/components/ui/button";
import { Menu, MenuPopup, MenuRadioGroup, MenuRadioItem, MenuTrigger } from "@/components/ui/menu";
import { getTheme, setTheme } from "@/lib/theme";

import type { IconProps } from "@/components/icons";
import type { ThemeChoice } from "@/lib/theme";
import type { ReactNode } from "react";

const CHOICES: { value: ThemeChoice; label: string; Icon: (props: IconProps) => ReactNode }[] = [
  { value: "system", label: "System", Icon: MonitorIcon },
  { value: "light", label: "Light", Icon: SunIcon },
  { value: "dark", label: "Dark", Icon: MoonIcon },
];

// setTheme always changes the root's data-theme attribute when the choice changes,
// which lets the menu follow a change made elsewhere, such as the T key.
function subscribe(onChange: () => void): () => void {
  const observer = new MutationObserver(onChange);
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  return () => observer.disconnect();
}

export function ThemeMenu() {
  const choice = useSyncExternalStore(subscribe, getTheme);
  const current = CHOICES.find((entry) => entry.value === choice) ?? CHOICES[0];

  return (
    <Menu>
      <MenuTrigger
        className={buttonStyles("ghost", "w-(--size-control) px-0")}
        title="Theme (T)"
        aria-label="Theme"
      >
        <current.Icon />
      </MenuTrigger>
      <MenuPopup side="bottom" align="end">
        <MenuRadioGroup value={choice} onValueChange={setTheme}>
          {CHOICES.map(({ value, label, Icon }) => (
            <MenuRadioItem key={value} value={value}>
              <Icon />
              {label}
            </MenuRadioItem>
          ))}
        </MenuRadioGroup>
      </MenuPopup>
    </Menu>
  );
}
