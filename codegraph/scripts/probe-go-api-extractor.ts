/**
 * Standalone probe — runs the generic Go API-endpoint extractor against
 * arbitrary `.go` files and prints the recovered registrations + client
 * calls. Used to validate the extractor against real-world codebases
 * before wiring it into the ingestion pipeline.
 *
 * Usage:
 *   npx tsx scripts/probe-go-api-extractor.ts <file.go> [<file.go> …]
 *
 * Example: prove that the kosmos↔weaver wire is statically recoverable:
 *   npx tsx scripts/probe-go-api-extractor.ts \
 *     /path/to/goserving/kosmos/web/routes/route.go \
 *     /path/to/goserving/cmpkg/abtest/abtestv2/internal/candidatesapi/candidatesapi.go
 */

import Parser from 'tree-sitter';
import Go from 'tree-sitter-go';
import * as fs from 'node:fs';
import { extractGoApiEndpoints } from '../src/core/ingestion/route-extractors/api-endpoint-go.js';

function probe(file: string) {
  const src = fs.readFileSync(file, 'utf-8');
  const parser = new Parser();
  parser.setLanguage(Go as any);
  const tree = parser.parse(src);
  const { routes, clientCalls } = extractGoApiEndpoints(tree.rootNode as any, file);
  console.log(`\n=== ${file} ===`);
  console.log(`  routes:        ${routes.length}`);
  for (const r of routes) {
    console.log(
      `    [${r.framework}] ${r.method.padEnd(7)} ${r.pathTemplate.padEnd(40)} -> ${r.handlerReceiver ? `${r.handlerReceiver}.` : ''}${r.handlerSymbol}`,
    );
  }
  console.log(`  client calls:  ${clientCalls.length}`);
  for (const c of clientCalls) {
    const tag = c.providerTag ? ` (tag=${c.providerTag})` : '';
    const path = c.pathLiteral ?? '<dynamic>';
    console.log(
      `    [${c.framework}] ${(c.method ?? '?').padEnd(7)} ${path.padEnd(40)} <- ${c.callerReceiver ? `${c.callerReceiver}.` : ''}${c.callerSymbol}${tag}`,
    );
  }
  return { routes, clientCalls };
}

function main() {
  const args = process.argv.slice(2);
  if (args.length === 0) {
    console.error('usage: probe-go-api-extractor <file.go> [<file.go> …]');
    process.exit(2);
  }
  const allRoutes: ReturnType<typeof probe>['routes'] = [];
  const allCalls: ReturnType<typeof probe>['clientCalls'] = [];
  for (const f of args) {
    const r = probe(f);
    allRoutes.push(...r.routes);
    allCalls.push(...r.clientCalls);
  }

  if (allRoutes.length > 0 && allCalls.length > 0) {
    console.log('\n=== reachable links (URL or path-literal join) ===');
    let links = 0;
    for (const c of allCalls) {
      if (!c.pathLiteral) continue;
      // Compare on the path portion (strip scheme/host if present).
      const callPath = c.pathLiteral.replace(/^https?:\/\/[^/]+/, '');
      for (const r of allRoutes) {
        if (
          callPath === r.pathTemplate ||
          callPath.endsWith(r.pathTemplate) ||
          r.pathTemplate.endsWith(callPath)
        ) {
          const methodOK = c.method == null || r.method === '*' || c.method === r.method;
          if (!methodOK) continue;
          links += 1;
          console.log(
            `  ${(c.callerReceiver ? c.callerReceiver + '.' : '') + c.callerSymbol} (${c.filePath})`,
          );
          console.log(`     --[${c.method ?? r.method}]--> ${r.pathTemplate}`);
          console.log(
            `     --HANDLED_BY--> ${(r.handlerReceiver ? r.handlerReceiver + '.' : '') + r.handlerSymbol} (${r.filePath})`,
          );
        }
      }
    }
    if (links === 0) {
      console.log('  no static URL link found between any client call and registered route');
    }
  }
}

main();
