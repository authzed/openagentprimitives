// Ambient module declaration so `tsc` accepts the side-effect CSS import in
// runtime/theme.tsx (`import "@ap/design/styles.css"`). Vite/esbuild handle CSS
// at build time; tsc only needs to know the specifier resolves to a module.
declare module "*.css";
