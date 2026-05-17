/**
 * Integration-flavoured tests for the Java options-bag → pending-getter
 * path. Mirrors the Go test of the same shape: exercise the full
 * extract → resolve chain through a single fixture so a regression in
 * any layer surfaces here.
 *
 * The motivating real-world shape is the Spring-style:
 *
 *     restTemplate.exchange(props.getKosmosUrl() + "/test", …)
 *
 * where `KosmosProps` carries `@Value("${kosmos.url}")` (or
 * `@ConfigurationProperties(prefix = "kosmos")`) and a JavaBean getter.
 */

import { describe, it, expect } from 'vitest';
import Parser from 'tree-sitter';
import Java from 'tree-sitter-java';
import { extractJavaApiEndpoints } from '../../src/core/ingestion/route-extractors/api-endpoint-java.js';
import {
  extractJavaConfigTags,
  extractJavaTrivialGetters,
} from '../../src/core/ingestion/route-extractors/config-tag-java.js';
import {
  buildResolvedGetters,
  getterKey,
} from '../../src/core/ingestion/route-extractors/config-tag-resolver.js';

function parseJava(src: string) {
  const parser = new Parser();
  parser.setLanguage(Java);
  return parser.parse(src);
}

describe('Java options-bag pending-getter resolution', () => {
  it('RestTemplate.exchange with non-literal URL emits pending lookup', () => {
    const tree = parseJava(`
      package x;
      import org.springframework.web.client.RestTemplate;
      import org.springframework.http.HttpMethod;
      public class Caller {
        private final RestTemplate rt;
        private final KosmosProps props;
        public void call() {
          rt.exchange(props.getKosmosUrl() + "/test", HttpMethod.POST, null, String.class);
        }
      }
    `);
    const api = extractJavaApiEndpoints(tree.rootNode, 'Caller.java');
    const c = api.clientCalls.find((c) => c.framework === 'spring.resttemplate');
    expect(c).toBeDefined();
    expect(c?.pathLiteral).toBeNull();
    const lookups = c?.pendingGetterLookups ?? [];
    expect(lookups.some((l) => l.name === 'getKosmosUrl')).toBe(true);
  });

  it('end-to-end fold: @ConfigurationProperties + JavaBean getter resolves the call site', () => {
    const tree = parseJava(`
      package x;

      @ConfigurationProperties(prefix = "kosmos")
      public class KosmosProps {
        private String url;
        public String getUrl() { return url; }
      }

      public class Caller {
        private RestTemplate rt;
        private KosmosProps props;
        public void call() {
          rt.getForObject(props.getUrl(), String.class);
        }
      }
    `);
    const api = extractJavaApiEndpoints(tree.rootNode, 'all.java');
    const c = api.clientCalls.find((c) => c.framework === 'spring.resttemplate');
    expect(c?.pendingGetterLookups?.some((l) => l.name === 'getUrl')).toBe(true);

    const tags = extractJavaConfigTags(tree.rootNode, 'all.java');
    const getters = extractJavaTrivialGetters(tree.rootNode, 'all.java');
    const resolved = buildResolvedGetters(tags, getters);

    const candidate = new Set<string>();
    for (const lk of c!.pendingGetterLookups!) {
      const a = resolved.get(getterKey(lk.receiver, lk.name));
      const b = resolved.get(getterKey(null, lk.name));
      if (a) for (const t of a) candidate.add(t);
      if (b) for (const t of b) candidate.add(t);
    }
    expect(candidate.has('kosmos')).toBe(true);
  });

  it('@Value-injected field + getter resolves through the SpEL prefix', () => {
    const tree = parseJava(`
      package x;
      public class Caller {
        @Value("\${weaver.url}")
        private String weaverUrl;
        public String getWeaverUrl() { return weaverUrl; }

        private RestTemplate rt;
        public void call() {
          rt.getForObject(getWeaverUrl(), String.class);
        }
      }
    `);
    const api = extractJavaApiEndpoints(tree.rootNode, 'Caller.java');
    const c = api.clientCalls.find((c) => c.framework === 'spring.resttemplate');
    const tags = extractJavaConfigTags(tree.rootNode, 'Caller.java');
    const getters = extractJavaTrivialGetters(tree.rootNode, 'Caller.java');
    const resolved = buildResolvedGetters(tags, getters);

    const all = new Set<string>();
    for (const lk of c!.pendingGetterLookups!) {
      const a = resolved.get(getterKey(lk.receiver, lk.name));
      const b = resolved.get(getterKey(null, lk.name));
      if (a) for (const t of a) all.add(t);
      if (b) for (const t of b) all.add(t);
    }
    expect(all.has('weaver')).toBe(true);
  });

  it('Lombok @Getter resolves via synthesised getter', () => {
    const tree = parseJava(`
      package x;

      @lombok.Getter
      @ConfigurationProperties(prefix = "abtest")
      public class AbtestProps {
        private String url;
      }

      public class Caller {
        private RestTemplate rt;
        private AbtestProps props;
        public void call() {
          rt.getForObject(props.getUrl(), String.class);
        }
      }
    `);
    const api = extractJavaApiEndpoints(tree.rootNode, 'all.java');
    const c = api.clientCalls.find((c) => c.framework === 'spring.resttemplate');
    const tags = extractJavaConfigTags(tree.rootNode, 'all.java');
    const getters = extractJavaTrivialGetters(tree.rootNode, 'all.java');
    const resolved = buildResolvedGetters(tags, getters);

    const all = new Set<string>();
    for (const lk of c!.pendingGetterLookups ?? []) {
      const v = resolved.get(getterKey(lk.receiver, lk.name));
      const w = resolved.get(getterKey(null, lk.name));
      if (v) for (const t of v) all.add(t);
      if (w) for (const t of w) all.add(t);
    }
    expect(all.has('abtest')).toBe(true);
  });
});
