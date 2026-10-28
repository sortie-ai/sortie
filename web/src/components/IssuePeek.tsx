import { useState } from "react";

import { IssueDetail } from "@/components/IssueDetail";
import { Drawer, DrawerPopup } from "@/components/ui/drawer";
import { useIssueSummary } from "@/components/use-issue-summary";
import { usePeek } from "@/components/use-peek";

interface PeekBodyProps {
  identifier: string;
}

function PeekBody({ identifier }: PeekBodyProps) {
  const row = useIssueSummary(identifier);
  return <IssueDetail identifier={identifier} row={row} peek />;
}

/** It keeps showing the last issue while it slides out. */
export function IssuePeek() {
  const { identifier, close } = usePeek();
  const [shown, setShown] = useState<string | null>(null);
  if (identifier !== null && identifier !== shown) {
    setShown(identifier);
  }

  function handleOpenChange(open: boolean) {
    if (!open) {
      close();
    }
  }

  return (
    <Drawer open={identifier !== null} onOpenChange={handleOpenChange}>
      <DrawerPopup>{shown === null ? undefined : <PeekBody identifier={shown} />}</DrawerPopup>
    </Drawer>
  );
}
