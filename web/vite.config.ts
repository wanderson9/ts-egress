import {defineConfig} from 'vite';

export default defineConfig({
 build: {
  outDir: '../internal/ui/dist', emptyOutDir: true,
  rollupOptions: {output: {entryFileNames: 'app.js', assetFileNames: 'app.[ext]'}},
 },
});
