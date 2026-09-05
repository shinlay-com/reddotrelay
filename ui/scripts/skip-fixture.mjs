// Isolated browser fixture: no Engine connection, secrets, or persistent writes.
// Run: node ui/scripts/skip-fixture.mjs; open http://127.0.0.1:8099/.
import { build } from "esbuild";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
const root = fileURLToPath(new URL("../", import.meta.url));
const result = await build({ absWorkingDir: root, stdin: { resolveDir: root, loader: "tsx", contents: `
import { render } from 'preact';
import { useState } from 'preact/hooks';
import { ScannerSkip } from './src/scanner-skip';
function Fixture() {
 const [paused,setPaused]=useState(false);const [revision,setRevision]=useState(1);
 return <main><h1>Isolated skip fixture</h1><button onClick={()=>{setPaused(!paused);setRevision(revision+1)}}>Toggle paused runtime</button><p>Runtime: {paused?'paused':'running'}</p><ScannerSkip session={{name:'fixture-admin',role:'admin',csrfToken:'fixture',expiresAt:''}} listener={{id:'fixture',paused}} revision={revision} runtimeState={paused?'paused':'running'} onChanged={()=>{setPaused(false);setRevision(revision+1)}} /></main>;
} render(<Fixture/>,document.body);
` }, bundle: true, write: false, format: "esm", jsx: "automatic", jsxImportSource: "preact" });
const css = await readFile(new URL("../src/styles.css", import.meta.url));
let audits = [];
createServer(async (req, res) => {
 res.setHeader("Cache-Control", "no-store");
 if(req.url==='/app.js'){res.setHeader('Content-Type','text/javascript');res.end(result.outputFiles[0].text);return;}
 if(req.url==='/styles.css'){res.setHeader('Content-Type','text/css');res.end(css);return;}
 if(req.url?.startsWith('/api/')) {
  res.setHeader('Content-Type','application/json');
  if(req.url.includes('skip-audit')){res.end(JSON.stringify({entries:audits}));return;}
  let body='';for await(const chunk of req)body+=chunk;
  const input=JSON.parse(body);const revision=Number(req.headers['if-match']?.match(/[0-9]+/)?.[0]);
  if(req.headers['x-csrf-token']!=='fixture'){res.writeHead(403);res.end('{}');return;}
  if(req.url.endsWith('/preview')){res.end(JSON.stringify({token:'fixture-token',configurationRevision:revision,previousBlock:1031532,fromBlock:1031533,toBlock:1053290,blocks:21758,confirmation:'SKIP 1053290',expiresAt:new Date(Date.now()+300000).toISOString()}));return;}
  if(input.token!=='fixture-token'||input.confirmation!=='SKIP 1053290'){res.writeHead(422);res.end('{}');return;}
  const audit={sequence:1,actorName:'fixture-admin',fromBlock:1031533,toBlock:1053290,createdAt:new Date().toISOString()};audits=[audit];res.end(JSON.stringify(audit));return;
 }
 res.setHeader('Content-Type','text/html');res.end('<!doctype html><html><head><meta name="viewport" content="width=device-width"><link rel="stylesheet" href="/styles.css"></head><body><script type="module" src="/app.js"></script></body></html>');
}).listen(8099,'127.0.0.1',()=>console.log('Isolated skip fixture: http://127.0.0.1:8099/'));
