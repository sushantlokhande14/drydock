import { useMemo } from "react";

interface DiffLine {
  kind: "add" | "del" | "context" | "hunk";
  oldNumber?: number;
  newNumber?: number;
  text: string;
}

interface DiffFile {
  path: string;
  lines: DiffLine[];
}

/** Parses a unified diff as `git diff` prints it. Anything it does not recognise is skipped, not guessed at. */
export function parseDiff(diff: string): DiffFile[] {
  const files: DiffFile[] = [];
  let current: DiffFile | null = null;
  let oldNumber = 0;
  let newNumber = 0;

  for (const line of diff.split("\n")) {
    if (line.startsWith("diff --git ")) {
      const match = line.match(/ b\/(.+)$/);
      current = { path: match ? match[1] : line, lines: [] };
      files.push(current);
      continue;
    }
    if (!current) continue;
    if (line.startsWith("@@")) {
      const match = line.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@(.*)$/);
      if (match) {
        oldNumber = Number(match[1]);
        newNumber = Number(match[2]);
        current.lines.push({ kind: "hunk", text: match[3].trim() });
      }
      continue;
    }
    if (line.startsWith("+++") || line.startsWith("---") || line.startsWith("index ") || line.startsWith("new file") || line.startsWith("deleted file") || line.startsWith("\\")) {
      continue;
    }
    if (line.startsWith("+")) {
      current.lines.push({ kind: "add", newNumber: newNumber++, text: line.slice(1) });
    } else if (line.startsWith("-")) {
      current.lines.push({ kind: "del", oldNumber: oldNumber++, text: line.slice(1) });
    } else if (line.startsWith(" ")) {
      current.lines.push({ kind: "context", oldNumber: oldNumber++, newNumber: newNumber++, text: line.slice(1) });
    }
  }
  return files;
}

export function Diff({ diff }: { diff: string }) {
  const files = useMemo(() => parseDiff(diff), [diff]);
  return (
    <div className="diff">
      {files.map((file) => (
        <section key={file.path} className="diff-file">
          <h3>{file.path}</h3>
          <table>
            <tbody>
              {file.lines.map((line, index) =>
                line.kind === "hunk" ? (
                  <tr key={index} className="hunk">
                    <td colSpan={3}>{line.text || "..."}</td>
                  </tr>
                ) : (
                  <tr key={index} className={line.kind}>
                    <td className="ln">{line.oldNumber ?? ""}</td>
                    <td className="ln">{line.newNumber ?? ""}</td>
                    <td className="code">
                      <span className="sign">{line.kind === "add" ? "+" : line.kind === "del" ? "-" : " "}</span>
                      {line.text}
                    </td>
                  </tr>
                ),
              )}
            </tbody>
          </table>
        </section>
      ))}
    </div>
  );
}
