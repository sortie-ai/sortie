import { BrandMark } from "@/app/BrandMark";
import { Link } from "@/components/carried-links";

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
