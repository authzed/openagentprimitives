// A schema.org JSON-LD block (lib/structured-data.ts). JSON.stringify leaves
// "<" alone, so a description containing "</script>" would end the element
// early; escaping it keeps the payload inert.
export function JsonLd({ data }: { data: object }) {
  return (
    <script
      type="application/ld+json"
      dangerouslySetInnerHTML={{
        __html: JSON.stringify(data).replace(/</g, "\\u003c"),
      }}
    />
  );
}
