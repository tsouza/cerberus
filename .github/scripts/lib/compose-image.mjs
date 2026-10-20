// compose-image.mjs — reads a pinned image reference out of a docker-compose
// file, so a script that runs a reference backend outside compose names the
// image the compose file pins rather than restating its tag.
//
// Exports:
//   composeImage(text, repository)       the first `image: <repository>:<tag>`
//                                        reference in compose text `text`, or
//                                        null when it names none.
//   readComposeImage(path, repository)   composeImage over the file at `path`;
//                                        throws when the file names none.

import { readFileSync } from 'node:fs';

export function composeImage(text, repository) {
  const escaped = repository.replace(/[|\\{}()[\]^$+*?.]/g, '\\$&');
  const m = new RegExp(`^\\s*image:\\s*["']?(${escaped}:[^\\s"'#]+)`, 'm').exec(text);
  return m ? m[1] : null;
}

export function readComposeImage(path, repository) {
  const image = composeImage(readFileSync(path, 'utf8'), repository);
  if (image === null) {
    throw new Error(`${path} pins no ${repository} image`);
  }
  return image;
}
