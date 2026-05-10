export type ManifestTemplateFile = {
  kind: string;
  namespace: string;
  name: string;
  yaml: string;
};

export type DiffRow = {
  type: "same" | "add" | "remove";
  text: string;
};

export const templatePath = (template: ManifestTemplateFile): string => {
  const ns = template.namespace.trim() === "" ? "cluster" : template.namespace.trim();
  return `${ns}/${template.kind.toLowerCase()}-${template.name}.yaml`;
};

export const templatesToPathMap = (templates: ManifestTemplateFile[]): Map<string, ManifestTemplateFile> => {
  const map = new Map<string, ManifestTemplateFile>();
  templates.forEach((template) => {
    map.set(templatePath(template), template);
  });
  return map;
};

export const splitLines = (input: string): string[] => input.replace(/\r\n/g, "\n").split("\n");

export const simpleDiff = (originalText: string, editedText: string): DiffRow[] => {
  const original = splitLines(originalText);
  const edited = splitLines(editedText);
  const max = Math.max(original.length, edited.length);
  const rows: DiffRow[] = [];
  for (let index = 0; index < max; index += 1) {
    const left = original[index];
    const right = edited[index];
    if (left === right) {
      rows.push({ type: "same", text: left ?? "" });
      continue;
    }
    if (left !== undefined) rows.push({ type: "remove", text: left });
    if (right !== undefined) rows.push({ type: "add", text: right });
  }
  return rows;
};

export const extractTemplateVariables = (line: string): string[] => {
  return line.match(/\{\{\s*\.[^}]+\s*\}\}/g) ?? [];
};
