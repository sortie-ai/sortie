import { Link } from "react-router";

import { BrandMark } from "@/app/BrandMark";

export function BrandLink() {
  return (
    <Link
      to="/"
      className="flex items-baseline gap-sm rounded-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus-ring"
    >
      <BrandMark />
      <span className="text-body-md">
        <span className="font-heading text-h7 leading-none font-semibold">Sortie</span>
      </span>
    </Link>
  );
}
