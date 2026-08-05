// Ambient declarations for the non-code imports the bundler resolves.
//
// `import "./index.css"` in main.tsx is a SIDE-EFFECT import: Vite extracts the
// file into the stylesheet and the module exports nothing at all. There is no
// value to name and nothing to type -- but TypeScript still has to be told the
// module exists.
//
// TypeScript 5 let this pass silently. TypeScript 7 reports it:
//
//   src/main.tsx(6,8): error TS2882: Cannot find module or type declarations
//                      for side-effect import of './index.css'.
//
// and is right to. Without a declaration, nothing distinguishes a stylesheet
// the bundler will handle from an import path that is simply wrong, so the
// typo case and the correct case looked identical to the type checker.
//
// DELIBERATELY NARROW. The usual answer is `/// <reference types="vite/client" />`,
// which works and also declares *.svg, *.png, *.jpg, *.worker, ?raw, ?url and
// more. Every one of those would then resolve whether or not the file exists,
// which trades one silent failure for a larger set of them. This interface
// imports exactly one kind of asset from TypeScript, so it declares exactly
// one; add a line here when that stops being true.
declare module "*.css" {}
