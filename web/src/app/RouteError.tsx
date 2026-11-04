import { ScreenHeader } from "@/components/ScreenHeader";
import { Link } from "@/components/carried-links";
import { Button, buttonStyles } from "@/components/ui/button";

/** A screen chunk fails to load when an upgraded binary has replaced the assets an open tab was built against; a reload fixes that. */
export function RouteError() {
  return (
    <div className="flex flex-col items-start">
      <ScreenHeader
        title="This screen could not be shown"
        standfirst="Reload the page to load the current version of the dashboard."
      />
      <div className="flex gap-xs">
        <Button variant="primary" onClick={() => window.location.reload()}>
          Reload
        </Button>
        <Link to="/" className={buttonStyles("secondary")}>
          Open Overview
        </Link>
      </div>
    </div>
  );
}
