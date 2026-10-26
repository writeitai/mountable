import { handleNote } from "../../../lib/request.ts";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";
export async function POST(request: Request) {
  return handleNote(request);
}
