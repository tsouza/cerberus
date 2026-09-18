// image-globs.mjs — the one image-ref glob every "skip these images" filter
// uses: k3d-image-import.mjs's IMAGE_IMPORT_EXCLUDE and pull-images.mjs's
// IMAGE_PULL_EXCLUDE (the two Justfile recipes that hand-rolled a `case …
// continue` around `_pull-retry` to skip the standalone `*-alpine` ClickHouse
// image now pass the same pattern to the same filter).
//
// A minimal single-`*` glob: `*` matches any run of characters EXCLUDING `/`,
// so a pattern never accidentally spans a repository-path boundary it did not
// ask to. Deliberately separate from ci-lane-contract.mjs's `matchesGlob`
// (file-path globs, `**` included): an image ref's `/`-and-`:`-delimited
// shape is a different domain.
//
// Exports:
//   globToRegExp(glob)                      the anchored RegExp for one pattern.
//   matchesAnyExcludePattern(img, patterns) true when `img` matches any pattern.
//   filterImages(images, patterns)          `images` minus every match, in order.

export function globToRegExp(glob) {
  const escaped = glob.replace(/[|\\{}()[\]^$+?.]/g, '\\$&').replace(/\*/g, '[^/]*');
  return new RegExp(`^${escaped}$`);
}

export function matchesAnyExcludePattern(img, patterns) {
  return patterns.some((p) => globToRegExp(p).test(img));
}

export function filterImages(images, excludePatterns) {
  if (excludePatterns.length === 0) return [...images];
  return images.filter((img) => !matchesAnyExcludePattern(img, excludePatterns));
}
