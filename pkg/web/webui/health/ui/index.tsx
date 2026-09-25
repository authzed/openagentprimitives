import { mount } from "@ap/runtime";
import { Card, CardHeader, CardTitle, CardContent } from "@ap/design";

function Health({ status = "ok" }: { status?: string }) {
  return (
    <div className="min-h-screen flex items-center justify-center bg-background text-foreground">
      <Card className="max-w-sm w-full">
        <CardHeader><CardTitle>webd</CardTitle></CardHeader>
        <CardContent><p className="text-sm text-muted-foreground">status: {status}</p></CardContent>
      </Card>
    </div>
  );
}

mount(Health);
