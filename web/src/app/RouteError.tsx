import { Link } from "react-router";

import { Button, buttonStyles } from "@/components/ui/button";

/** A screen chunk fails to load when an upgraded binary has replaced the assets an open tab was built against, and a reload fixes that. */
export function RouteError() {
  return (
    <div className="grid justify-items-start gap-md">
      <h1 className="font-heading text-h6 font-semibold">This screen could not be shown.</h1>
      <p>Reload the page to load the current version of the dashboard.</p>
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
