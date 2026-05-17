/**
 * Java config-tag + trivial-getter extractors.
 *
 * Mirrors the Go pair in `config-tag-resolver.ts` but recognises the
 * Spring / Lombok / Jakarta-CDI annotation surface instead of struct
 * tags. Together with `buildResolvedGetters` (language-agnostic fold)
 * this lets non-literal HTTP client URLs of the form
 * `restTemplate.exchange(props.getKosmosUrl() + "/test", …)` recover
 * a `providerTag` purely from static AST evidence.
 *
 * Three patterns covered, all repo-agnostic:
 *
 *   1. **`@Value("${prefix.path}")` field injection** — a Spring
 *      bean injects a config value into a private field. We bind
 *      the field to the SpEL prefix so any getter that returns it
 *      can be resolved to the same tag as a YAML key, an env-var,
 *      or a properties line.
 *
 *   2. **`@ConfigurationProperties(prefix = "tag")` class binding**
 *      — every field of the annotated class becomes implicitly
 *      bound to `tag`. We treat this as if every field had its own
 *      `@Value("${tag}")` annotation.
 *
 *   3. **Lombok `@Getter`** — the field is exposed as a synthesised
 *      `getXxx()` method. Lombok runs at compile time so the AST
 *      doesn't *show* the method body, but for our purposes we can
 *      synthesise an equivalent {@link TrivialGetterBinding} that
 *      points at the same field name.
 *
 * Trivial-getter extraction is the standard JavaBean pattern:
 *   `public T getX()` / `public T isX()` / `public T getX_y()` whose
 *   body is just `return <expr>;` (any number of returns, including
 *   branches). Both `return x;` and `return this.x;` are supported.
 */

import type { SyntaxNode } from '../utils/ast-helpers.js';
import type {
  ConfigTagBinding,
  TrivialGetterBinding,
} from './config-tag-resolver.js';
import { tagFromKeyPath } from './provider-resolver.js';

// ─────────────────────────────────────────────────────────────────
// AST helpers
// ─────────────────────────────────────────────────────────────────

function* walk(root: SyntaxNode): Generator<SyntaxNode> {
  const stack: SyntaxNode[] = [root];
  while (stack.length > 0) {
    const node = stack.pop()!;
    yield node;
    const children = node.children ?? [];
    for (let i = children.length - 1; i >= 0; i--) {
      const c = children[i];
      if (c?.isNamed === false) continue;
      stack.push(c);
    }
  }
}

/** Read the body of a `string_literal` node. Strips the surrounding
 *  quotes via the inner `string_fragment` child when present (the
 *  preferred shape under tree-sitter-java's modern grammar). */
function readJavaString(node: SyntaxNode | null | undefined): string | null {
  if (!node) return null;
  if (node.type !== 'string_literal') return null;
  const frag = node.namedChildren?.find((c) => c.type === 'string_fragment');
  if (frag?.text) return frag.text;
  // Fallback for older grammar: strip outer quotes manually.
  const t = node.text ?? '';
  if (t.length < 2) return null;
  if ((t.startsWith('"') && t.endsWith('"')) || (t.startsWith("'") && t.endsWith("'"))) {
    return t.slice(1, -1);
  }
  return null;
}

/** Strip the `${…}` SpEL wrapper and any `:default` suffix.
 *  Returns the inner key path string, or null when the input isn't
 *  a SpEL placeholder. */
function unwrapSpel(value: string): string | null {
  const v = value.trim();
  if (!v.startsWith('${') || !v.endsWith('}')) return null;
  const inner = v.slice(2, -1);
  // Drop default-value suffix `:foo`, but be careful with URL-shaped
  // defaults (`https://…`) — the colon there is part of the value.
  // Heuristic: only strip on the first `:` whose RHS doesn't look
  // like a URL scheme continuation (`//`).
  const colon = inner.indexOf(':');
  if (colon <= 0) return inner;
  const rest = inner.slice(colon + 1);
  if (rest.startsWith('//') || /^[a-z]+:/.test(rest)) return inner;
  return inner.slice(0, colon);
}

/** Iterate every annotation node attached to `modifiers`. */
function* iterAnnotations(modifiers: SyntaxNode | null | undefined): Generator<SyntaxNode> {
  if (!modifiers) return;
  for (const c of modifiers.namedChildren ?? []) {
    if (c.type === 'annotation' || c.type === 'marker_annotation') yield c;
  }
}

/** Get the bare annotation name, with package qualifier stripped
 *  (`org.springframework.beans.factory.annotation.Value` → `Value`). */
function annotationName(annot: SyntaxNode): string | null {
  const n = annot.childForFieldName?.('name');
  if (!n) return null;
  const t = n.text ?? '';
  const dot = t.lastIndexOf('.');
  return dot >= 0 ? t.slice(dot + 1) : t;
}

/** Look up an annotation argument by key.
 *
 *   `@FeignClient(name = "kosmos")` ⇒ `lookup(annot, "name")` → `"kosmos"`
 *   `@Value("${kosmos.url}")`        ⇒ `lookup(annot, null)`  → `"${kosmos.url}"`
 */
function annotationArgString(annot: SyntaxNode, key: string | null): string | null {
  const args = annot.childForFieldName?.('arguments');
  if (!args) return null;
  for (const child of args.namedChildren ?? []) {
    if (child.type === 'string_literal') {
      // Bare positional argument (only used when key === null).
      if (key === null) return readJavaString(child);
    } else if (child.type === 'element_value_pair') {
      const namedChildren = child.namedChildren ?? [];
      if (namedChildren.length < 2) continue;
      const ident = namedChildren[0];
      const value = child.childForFieldName?.('value') ?? namedChildren[1];
      if (ident.text === key) return readJavaString(value);
    }
  }
  return null;
}

/** Find the enclosing `class_declaration` / `interface_declaration` /
 *  `record_declaration` and return its declared name. */
function enclosingClassName(node: SyntaxNode): string | null {
  let cur: SyntaxNode | null = node.parent;
  while (cur) {
    if (
      cur.type === 'class_declaration' ||
      cur.type === 'interface_declaration' ||
      cur.type === 'record_declaration' ||
      cur.type === 'enum_declaration'
    ) {
      return cur.childForFieldName?.('name')?.text ?? null;
    }
    cur = cur.parent;
  }
  return null;
}

/** Capitalise the first character (`x` → `X`). */
function capitalise(s: string): string {
  if (!s) return s;
  return s[0].toUpperCase() + s.slice(1);
}

/** Walk a Java expression and flatten it into an alias path,
 *  matching the shape consumed by `buildResolvedGetters`.
 *
 *   `x`                 → `["x"]`
 *   `this.x`            → `["this", "x"]` *
 *   `this.x.y`          → `["this", "x", "y"]`
 *   `getX()`            → `["getX"]`
 *   `props.getX()`      → `["props", "getX"]`
 *   `props.getX().y`    → `["props", "getX", "y"]`
 *
 *  *NB:* the resolver's BFS will try `("*", "x")` after `("this","x")`,
 *  so the leading `this` doesn't break field-tag binding.
 */
function flattenAlias(node: SyntaxNode | null | undefined): string[] | null {
  if (!node) return null;
  if (node.type === 'identifier' || node.type === 'type_identifier') {
    return node.text ? [node.text] : null;
  }
  if (node.type === 'this') return ['this'];
  if (node.type === 'field_access') {
    // `<obj>.<field>` — `field` is a field-named child.
    const obj = node.childForFieldName?.('object');
    const field = node.childForFieldName?.('field');
    const left = flattenAlias(obj);
    if (!left) return null;
    if (!field?.text) return null;
    return [...left, field.text];
  }
  if (node.type === 'method_invocation') {
    // `[<obj>.]<name>(…)` — drop the args, keep the chain.
    const obj = node.childForFieldName?.('object');
    const name = node.childForFieldName?.('name');
    if (!name?.text) return null;
    if (obj) {
      const left = flattenAlias(obj);
      if (!left) return null;
      return [...left, name.text];
    }
    return [name.text];
  }
  if (node.type === 'parenthesized_expression') {
    const inner = node.namedChildren?.[0] ?? null;
    return flattenAlias(inner);
  }
  if (node.type === 'cast_expression') {
    const value = node.childForFieldName?.('value') ?? null;
    return flattenAlias(value);
  }
  return null;
}

/** Iterate every top-level `return_statement` reachable without
 *  descending into a nested function body (lambda / class body /
 *  inner method). */
function* iterReturns(body: SyntaxNode): Generator<SyntaxNode> {
  const stack: SyntaxNode[] = [body];
  while (stack.length > 0) {
    const cur = stack.pop()!;
    if (
      cur.type === 'method_declaration' ||
      cur.type === 'lambda_expression' ||
      cur.type === 'class_body'
    ) {
      // Don't cross into a nested method / lambda / inner class.
      if (cur === body) {
        // ...except when we *started* at this body.
      } else continue;
    }
    if (cur.type === 'return_statement') {
      yield cur;
      continue;
    }
    const children = cur.children ?? [];
    for (let i = children.length - 1; i >= 0; i--) {
      const c = children[i];
      if (c?.isNamed === false) continue;
      stack.push(c);
    }
  }
}

// ─────────────────────────────────────────────────────────────────
// Public extractors
// ─────────────────────────────────────────────────────────────────

/** Walk a Java file's AST and emit a {@link ConfigTagBinding} for
 *  every:
 *    - field annotated with `@Value("${prefix.path}")`
 *    - field of a class annotated with `@ConfigurationProperties(prefix = "tag")`
 *
 *  The owner is the enclosing class name when known, `"*"` otherwise.
 *  Tag values are normalised through {@link tagFromKeyPath} so they
 *  match the YAML / properties / env tags emitted by
 *  `provider-resolver.ts`. */
export function extractJavaConfigTags(
  rootNode: SyntaxNode,
  filePath: string,
): ConfigTagBinding[] {
  const out: ConfigTagBinding[] = [];

  for (const node of walk(rootNode)) {
    // Pattern 1: class-level @ConfigurationProperties(prefix = "tag")
    if (
      node.type === 'class_declaration' ||
      node.type === 'interface_declaration' ||
      node.type === 'record_declaration'
    ) {
      const modifiers = node.namedChildren?.find((c) => c.type === 'modifiers');
      const className = node.childForFieldName?.('name')?.text;
      if (!className) continue;
      let classTag: string | null = null;
      for (const annot of iterAnnotations(modifiers)) {
        const an = annotationName(annot);
        if (an === 'ConfigurationProperties' || an === 'ConditionalOnProperty') {
          // Allow either `@ConfigurationProperties("tag")` or
          // `@ConfigurationProperties(prefix = "tag")` / `value = "tag"`.
          const raw =
            annotationArgString(annot, 'prefix') ??
            annotationArgString(annot, 'value') ??
            annotationArgString(annot, null);
          if (!raw) continue;
          const path = raw.split('.').filter((s) => s.length > 0);
          const t = tagFromKeyPath(path);
          if (t) classTag = t;
        }
      }
      if (classTag) {
        // Bind every field of the class to the class tag. The body
        // is a `class_body` field-named child.
        const body = node.childForFieldName?.('body');
        if (!body) continue;
        for (const member of body.namedChildren ?? []) {
          if (member.type !== 'field_declaration') continue;
          for (const decl of member.namedChildren ?? []) {
            if (decl.type !== 'variable_declarator') continue;
            const fieldName = decl.childForFieldName?.('name')?.text;
            if (!fieldName) continue;
            out.push({
              owner: className,
              field: fieldName,
              tags: { java: classTag },
              filePath,
              lineNumber: member.startPosition?.row ?? 0,
            });
          }
        }
      }
    }

    // Pattern 2: per-field @Value("${prefix.path}")
    if (node.type === 'field_declaration') {
      const modifiers = node.namedChildren?.find((c) => c.type === 'modifiers');
      let valueTag: string | null = null;
      let lombokGetterPresent = false;
      for (const annot of iterAnnotations(modifiers)) {
        const an = annotationName(annot);
        if (an === 'Value') {
          const raw = annotationArgString(annot, null);
          if (!raw) continue;
          const inner = unwrapSpel(raw) ?? raw;
          const path = inner.split('.').filter((s) => s.length > 0);
          const t = tagFromKeyPath(path);
          if (t) valueTag = t;
        } else if (an === 'Getter') {
          lombokGetterPresent = true;
        }
      }
      if (!valueTag && !lombokGetterPresent) continue;

      const owner = enclosingClassName(node) ?? '*';
      for (const decl of node.namedChildren ?? []) {
        if (decl.type !== 'variable_declarator') continue;
        const fieldName = decl.childForFieldName?.('name')?.text;
        if (!fieldName) continue;
        if (valueTag) {
          out.push({
            owner,
            field: fieldName,
            tags: { java: valueTag },
            filePath,
            lineNumber: node.startPosition?.row ?? 0,
          });
        }
      }
    }
  }

  return out;
}

/** Walk a Java file's AST and emit a {@link TrivialGetterBinding} for:
 *    - every method whose body is just `return <expr>;` (one or more,
 *      including branched returns), AND
 *    - synthetic getters for every field annotated `@lombok.Getter`
 *      or whose enclosing class is annotated `@lombok.Getter` (Lombok
 *      runs at compile-time so the AST has no method body to walk —
 *      we synthesise the canonical `return field;` shape instead). */
export function extractJavaTrivialGetters(
  rootNode: SyntaxNode,
  filePath: string,
): TrivialGetterBinding[] {
  const out: TrivialGetterBinding[] = [];

  // Pass 1 — actual method bodies that look like trivial getters.
  for (const node of walk(rootNode)) {
    if (node.type !== 'method_declaration') continue;
    const name = node.childForFieldName?.('name')?.text;
    if (!name) continue;
    const body = node.childForFieldName?.('body');
    if (!body || body.type !== 'block') continue;

    const aliases: string[][] = [];
    for (const ret of iterReturns(body)) {
      // `return_statement` may have either:
      //   - children: `return`, <expr>, `;`
      //   - or be empty (`return;`) — skip
      const expr = ret.namedChildren?.[0];
      if (!expr) continue;
      const alias = flattenAlias(expr);
      if (!alias || alias.length === 0) continue;
      aliases.push(alias);
    }
    if (aliases.length === 0) continue;

    out.push({
      name,
      receiver: enclosingClassName(node),
      returnAliases: aliases,
      filePath,
      lineNumber: node.startPosition?.row ?? 0,
    });
  }

  // Pass 2 — Lombok @Getter on a class or a field synthesises
  // canonical `getX() { return x; }` accessors. We emit synthetic
  // bindings for them so the resolver can chase aliases through
  // Lombok-generated getters.
  for (const node of walk(rootNode)) {
    if (
      node.type !== 'class_declaration' &&
      node.type !== 'record_declaration'
    ) {
      continue;
    }
    const className = node.childForFieldName?.('name')?.text;
    if (!className) continue;
    const classModifiers = node.namedChildren?.find((c) => c.type === 'modifiers');
    let classHasGetter = false;
    for (const annot of iterAnnotations(classModifiers)) {
      const an = annotationName(annot);
      if (an === 'Getter' || an === 'Data') classHasGetter = true;
    }
    const body = node.childForFieldName?.('body');
    if (!body) continue;
    for (const member of body.namedChildren ?? []) {
      if (member.type !== 'field_declaration') continue;
      const fieldModifiers = member.namedChildren?.find((c) => c.type === 'modifiers');
      let fieldHasGetter = classHasGetter;
      for (const annot of iterAnnotations(fieldModifiers)) {
        if (annotationName(annot) === 'Getter') fieldHasGetter = true;
      }
      if (!fieldHasGetter) continue;
      for (const decl of member.namedChildren ?? []) {
        if (decl.type !== 'variable_declarator') continue;
        const fieldName = decl.childForFieldName?.('name')?.text;
        if (!fieldName) continue;
        // Lombok mints `getFoo` for non-boolean and `isFoo` for
        // boolean — without the Java type system we can't tell, so
        // emit both shapes. Cheap (one extra entry per field).
        for (const prefix of ['get', 'is']) {
          out.push({
            name: prefix + capitalise(fieldName),
            receiver: className,
            returnAliases: [['this', fieldName]],
            filePath,
            lineNumber: member.startPosition?.row ?? 0,
          });
        }
      }
    }
  }

  return out;
}
