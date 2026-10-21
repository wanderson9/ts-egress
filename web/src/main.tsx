import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { Setup } from './Setup';
import { Dashboard } from './Dashboard';
import { Settings } from './Settings';
import { Footer } from './Footer';
import './style.css';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <main>
      {document.body.dataset.page === 'settings' ? (
        <Settings />
      ) : document.body.dataset.page === 'dashboard' ? (
        <Dashboard />
      ) : (
        <Setup />
      )}
    </main>
    <Footer />
  </StrictMode>,
);
