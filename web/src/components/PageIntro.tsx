/** Standard page heading and lead paragraph. */
export function PageIntro({
  title,
  description,
}: {
  title: string;
  description: string;
}) {
  return (
    <header>
      <h2 className="text-xl font-semibold tracking-tight">{title}</h2>
      <p className="mt-2 max-w-prose text-sm text-content-muted">{description}</p>
    </header>
  );
}
