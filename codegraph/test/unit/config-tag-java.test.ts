/**
 * Unit tests for the Java config-tag + trivial-getter extractors.
 *
 * The shared resolver fold (`buildResolvedGetters`) is covered by
 * `config-tag-resolver.test.ts`; here we focus on the Java-specific
 * extraction surface — `@Value` SpEL parsing,
 * `@ConfigurationProperties` class binding, JavaBean trivial getters
 * (with `this.x` and `x` shapes), and Lombok `@Getter` synthesis.
 */

import { describe, it, expect } from 'vitest';
import Parser from 'tree-sitter';
import Java from 'tree-sitter-java';
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

// ─────────────────────────────────────────────────────────────────
// extractJavaConfigTags
// ─────────────────────────────────────────────────────────────────

describe('extractJavaConfigTags', () => {
  it('captures @Value("${prefix.path}") on a field', () => {
    const tree = parseJava(`
      package x;
      public class Foo {
        @Value("\${kosmos.url}")
        private String kosmosUrl;
      }
    `);
    const tags = extractJavaConfigTags(tree.rootNode, 'Foo.java');
    expect(tags).toHaveLength(1);
    expect(tags[0].owner).toBe('Foo');
    expect(tags[0].field).toBe('kosmosUrl');
    expect(tags[0].tags.java).toBe('kosmos');
  });

  it('strips :default suffix in SpEL', () => {
    const tree = parseJava(`
      package x;
      public class Foo {
        @Value("\${abtest.api.host:localhost}")
        private String host;
      }
    `);
    const tags = extractJavaConfigTags(tree.rootNode, 'Foo.java');
    expect(tags[0].tags.java).toBe('abtest');
  });

  it('keeps URL-shaped defaults intact (does not split on `:` after //)', () => {
    const tree = parseJava(`
      package x;
      public class Foo {
        @Value("\${weaver.url:https://default.example/api}")
        private String url;
      }
    `);
    const tags = extractJavaConfigTags(tree.rootNode, 'Foo.java');
    expect(tags[0].tags.java).toBe('weaver');
  });

  it('binds every field of @ConfigurationProperties(prefix = "tag")', () => {
    const tree = parseJava(`
      package x;
      @ConfigurationProperties(prefix = "kosmos")
      public class KosmosProps {
        private String host;
        private String path;
        private int port;
      }
    `);
    const tags = extractJavaConfigTags(tree.rootNode, 'KosmosProps.java');
    expect(tags).toHaveLength(3);
    for (const t of tags) {
      expect(t.owner).toBe('KosmosProps');
      expect(t.tags.java).toBe('kosmos');
    }
    expect(tags.map((t) => t.field).sort()).toEqual(['host', 'path', 'port']);
  });

  it('also accepts @ConfigurationProperties("tag") positional form', () => {
    const tree = parseJava(`
      package x;
      @ConfigurationProperties("weaver")
      public class W {
        private String url;
      }
    `);
    const tags = extractJavaConfigTags(tree.rootNode, 'W.java');
    expect(tags[0].tags.java).toBe('weaver');
  });

  it('skips fields with no recognised annotation', () => {
    const tree = parseJava(`
      package x;
      public class Foo {
        private String regularField;
        @Value("\${kosmos.url}") private String kosmosUrl;
      }
    `);
    const tags = extractJavaConfigTags(tree.rootNode, 'Foo.java');
    expect(tags).toHaveLength(1);
    expect(tags[0].field).toBe('kosmosUrl');
  });
});

// ─────────────────────────────────────────────────────────────────
// extractJavaTrivialGetters
// ─────────────────────────────────────────────────────────────────

describe('extractJavaTrivialGetters', () => {
  it('captures `return field;` getter', () => {
    const tree = parseJava(`
      package x;
      public class Foo {
        public String getKosmosUrl() { return kosmosUrl; }
      }
    `);
    const getters = extractJavaTrivialGetters(tree.rootNode, 'Foo.java');
    const g = getters.find((g) => g.name === 'getKosmosUrl');
    expect(g).toBeDefined();
    expect(g?.receiver).toBe('Foo');
    expect(g?.returnAliases).toEqual([['kosmosUrl']]);
  });

  it('captures `return this.field;` getter', () => {
    const tree = parseJava(`
      package x;
      public class Foo {
        public String getHost() { return this.host; }
      }
    `);
    const getters = extractJavaTrivialGetters(tree.rootNode, 'Foo.java');
    const g = getters.find((g) => g.name === 'getHost');
    expect(g?.returnAliases).toEqual([['this', 'host']]);
  });

  it('captures branched returns', () => {
    const tree = parseJava(`
      package x;
      public class Foo {
        public String getKosmos() {
          if (prod) { return prodCfg.host; }
          return stagingCfg.host;
        }
      }
    `);
    const getters = extractJavaTrivialGetters(tree.rootNode, 'Foo.java');
    const g = getters.find((g) => g.name === 'getKosmos');
    expect(g?.returnAliases).toContainEqual(['prodCfg', 'host']);
    expect(g?.returnAliases).toContainEqual(['stagingCfg', 'host']);
  });

  it('captures method-call alias (one getter calls another)', () => {
    const tree = parseJava(`
      package x;
      public class Foo {
        public String getDeprecated() { return getKosmos(); }
        public String getKosmos() { return kosmosUrl; }
      }
    `);
    const getters = extractJavaTrivialGetters(tree.rootNode, 'Foo.java');
    const dep = getters.find((g) => g.name === 'getDeprecated');
    expect(dep?.returnAliases).toEqual([['getKosmos']]);
  });

  it('synthesises Lombok @Getter on a field', () => {
    const tree = parseJava(`
      package x;
      public class Foo {
        @Getter private String kosmosUrl;
      }
    `);
    const getters = extractJavaTrivialGetters(tree.rootNode, 'Foo.java');
    expect(getters.some((g) => g.name === 'getKosmosUrl' && g.receiver === 'Foo')).toBe(
      true,
    );
    // Both `getXxx` and `isXxx` are emitted (we don't know the
    // field type), so they'll both be present.
    expect(getters.some((g) => g.name === 'isKosmosUrl')).toBe(true);
  });

  it('synthesises Lombok @Getter applied at class level (covers all fields)', () => {
    const tree = parseJava(`
      package x;
      @Getter
      public class Foo {
        private String host;
        private int port;
      }
    `);
    const getters = extractJavaTrivialGetters(tree.rootNode, 'Foo.java');
    const names = new Set(getters.map((g) => g.name));
    expect(names.has('getHost')).toBe(true);
    expect(names.has('getPort')).toBe(true);
  });
});

// ─────────────────────────────────────────────────────────────────
// End-to-end fold
// ─────────────────────────────────────────────────────────────────

describe('Java config-tag fold via buildResolvedGetters', () => {
  it('@Value field + getter resolve to the SpEL prefix tag', () => {
    const tree = parseJava(`
      package x;
      public class Props {
        @Value("\${kosmos.url}")
        private String kosmosUrl;
        public String getKosmosUrl() { return this.kosmosUrl; }
      }
    `);
    const tags = extractJavaConfigTags(tree.rootNode, 'Props.java');
    const getters = extractJavaTrivialGetters(tree.rootNode, 'Props.java');
    const resolved = buildResolvedGetters(tags, getters);

    const v = resolved.get(getterKey('Props', 'getKosmosUrl'));
    expect(v).toBeDefined();
    expect(v?.has('kosmos')).toBe(true);
  });

  it('@ConfigurationProperties + JavaBean getter chain resolves', () => {
    const tree = parseJava(`
      package x;
      @ConfigurationProperties(prefix = "abtest")
      public class AbtestProps {
        private String host;
        public String getHost() { return host; }
      }
    `);
    const tags = extractJavaConfigTags(tree.rootNode, 'AbtestProps.java');
    const getters = extractJavaTrivialGetters(tree.rootNode, 'AbtestProps.java');
    const resolved = buildResolvedGetters(tags, getters);

    const v = resolved.get(getterKey('AbtestProps', 'getHost'));
    expect(v?.has('abtest')).toBe(true);
  });

  it('Lombok @Getter resolves through synthesised method', () => {
    const tree = parseJava(`
      package x;
      @ConfigurationProperties(prefix = "weaver")
      @Getter
      public class WeaverProps {
        private String url;
      }
    `);
    const tags = extractJavaConfigTags(tree.rootNode, 'WeaverProps.java');
    const getters = extractJavaTrivialGetters(tree.rootNode, 'WeaverProps.java');
    const resolved = buildResolvedGetters(tags, getters);

    const v = resolved.get(getterKey('WeaverProps', 'getUrl'));
    expect(v?.has('weaver')).toBe(true);
  });
});
