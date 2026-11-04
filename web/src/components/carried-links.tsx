import { useMemo } from "react";
import {
  Link as RouterLink,
  NavLink as RouterNavLink,
  useLocation,
  useNavigate as useRouterNavigate,
} from "react-router";

import { withCarriedUnits } from "@/lib/units";

import type { ComponentProps } from "react";
import type { NavigateFunction, NavigateOptions, To } from "react-router";

/** Renders a router link whose target keeps the address's `units`. */
export function Link({ to, ...props }: ComponentProps<typeof RouterLink>) {
  const { search } = useLocation();
  return <RouterLink to={withCarriedUnits(to, search)} {...props} />;
}

/** Renders a router nav link whose target keeps the address's `units`. */
export function NavLink({ to, ...props }: ComponentProps<typeof RouterNavLink>) {
  const { search } = useLocation();
  return <RouterNavLink to={withCarriedUnits(to, search)} {...props} />;
}

/** Returns a navigate function whose target keeps the address's `units`; a history delta passes through. */
export function useNavigate(): NavigateFunction {
  const navigate = useRouterNavigate();
  const { search } = useLocation();
  return useMemo(() => {
    function carriedNavigate(to: To, options?: NavigateOptions): void | Promise<void>;
    function carriedNavigate(delta: number): void | Promise<void>;
    function carriedNavigate(to: To | number, options?: NavigateOptions): void | Promise<void> {
      if (typeof to === "number") {
        return navigate(to);
      }
      return navigate(withCarriedUnits(to, search), options);
    }
    return carriedNavigate;
  }, [navigate, search]);
}
