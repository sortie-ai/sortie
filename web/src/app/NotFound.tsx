import { Link, useLocation } from "react-router";

import { ScreenHeader } from "@/components/ScreenHeader";
import { buttonStyles } from "@/components/ui/button";

export function NotFound() {
  const { pathname } = useLocation();

  return (
    <div className="flex flex-col items-start">
      <ScreenHeader
        title="This page does not exist"
        standfirst={`No screen of the dashboard answers to ${pathname}.`}
      />
      <Link to="/" className={buttonStyles("secondary")}>
        Open Overview
      </Link>
    </div>
  );
}
