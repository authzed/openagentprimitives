import { Card, CardContent, CardHeader, CardTitle } from "@ap/design";
import { VIEW_META, type ViewId } from "./nav";

interface PlaceholderProps {
  view: ViewId;
}

export function Placeholder({ view }: PlaceholderProps) {
  const meta = VIEW_META[view];
  return (
    <Card className="mt-2 border-dashed">
      <CardHeader>
        <CardTitle className="text-sm font-medium text-muted-foreground">{meta.title}</CardTitle>
      </CardHeader>
      <CardContent>
        <p className="text-xs text-muted-foreground">
          This surface lands in a later Part 7 task.
        </p>
      </CardContent>
    </Card>
  );
}
