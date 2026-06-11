#!/usr/bin/env node
'use strict';

// devrouter codegraph sidecar CLI.
//
//   codegraph-sidecar serve [--port N]      start the HTTP API (default 4747)
//   codegraph-sidecar index <repo> [--name] index/refresh a repo + register it
//   codegraph-sidecar repos                 print the registry

function parseFlags(args) {
  const flags = {};
  const positional = [];
  for (let i = 0; i < args.length; i++) {
    const a = args[i];
    if (a.startsWith('--')) {
      const eq = a.indexOf('=');
      if (eq !== -1) { flags[a.slice(2, eq)] = a.slice(eq + 1); }
      else if (i + 1 < args.length && !args[i + 1].startsWith('--')) { flags[a.slice(2)] = args[++i]; }
      else { flags[a.slice(2)] = true; }
    } else {
      positional.push(a);
    }
  }
  return { flags, positional };
}

async function main() {
  const [cmd, ...rest] = process.argv.slice(2);
  const { flags, positional } = parseFlags(rest);

  switch (cmd) {
    case 'serve': {
      const port = Number(flags.port || process.env.CODEGRAPH_PORT || 4747);
      require('../lib/server').serve(port);
      break;
    }
    case 'index': {
      const repo = positional[0];
      if (!repo) { console.error('Usage: codegraph-sidecar index <repoPath> [--name <name>]'); process.exit(1); }
      await require('../lib/indexer').indexRepo(repo, flags.name);
      break;
    }
    case 'repos': {
      const reg = require('../lib/registry').loadRegistry();
      console.log(JSON.stringify(reg, null, 2));
      break;
    }
    default:
      console.error('Commands: serve [--port N] | index <repo> [--name X] | repos');
      process.exit(1);
  }
}

main().catch((e) => { console.error(e); process.exit(1); });
