import { build } from "esbuild";
import { mkdir, readFile, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

await rm(path.join(root, "dist"), { recursive: true, force: true });
await mkdir(path.join(root, "dist"), { recursive: true });

const result = await build({
  absWorkingDir: root,
  entryPoints: ["src/app.tsx"],
  outdir: "dist/assets",
  entryNames: "[name]-[hash]",
  bundle: true,
  minify: true,
  sourcemap: false,
  metafile: true,
  format: "esm",
  target: ["es2022"],
  jsx: "automatic",
  jsxImportSource: "preact"
});

let scriptPath = "";
let stylePath = "";
for (const [file, metadata] of Object.entries(result.metafile.outputs)) {
  const relative = path.relative(path.join(root, "dist"), path.resolve(root, file)).replaceAll("\\", "/");
  if (metadata.entryPoint?.endsWith("app.tsx") && file.endsWith(".js")) scriptPath = `/ui/${relative}`;
  if (file.endsWith(".css")) stylePath = `/ui/${relative}`;
}
if (!scriptPath || !stylePath) throw new Error("UI build did not produce JavaScript and CSS assets");

const template = await readFile(path.join(root, "index.html"), "utf8");
await writeFile(
  path.join(root, "dist", "index.html"),
  template.replace("{{SCRIPT_PATH}}", scriptPath).replace("{{STYLE_PATH}}", stylePath)
);
