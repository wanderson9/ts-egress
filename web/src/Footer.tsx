import { useEffect, useState } from 'react';

const repo = 'https://github.com/wanderson9/ts-egress';
export function Footer() {
  const [release, setRelease] = useState<{
    version: string;
    latest?: string;
    updateAvailable: boolean;
  }>();
  useEffect(() => {
    let active = true;
    const check = () =>
      fetch('/api/version', { credentials: 'same-origin' })
        .then(async (response) => {
          if (!response.ok) return;
          const value = await response.json();
          if (active) setRelease(value);
        })
        .catch(() => {});
    void check();
    const timer = setInterval(check, 60000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, []);
  return (
    <footer className="app-footer">
      <a href={repo} target="_blank" rel="noopener noreferrer" aria-label="ts-egress on GitHub">
        <svg width="18" height="18" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
          <path d="M12 .75a11.25 11.25 0 0 0-3.558 21.922c.563.104.77-.244.77-.542 0-.267-.01-.975-.015-1.914-3.13.68-3.79-1.508-3.79-1.508-.512-1.3-1.25-1.646-1.25-1.646-1.022-.699.078-.685.078-.685 1.13.08 1.725 1.16 1.725 1.16 1.005 1.723 2.637 1.225 3.28.937.102-.728.393-1.225.715-1.507-2.498-.284-5.124-1.249-5.124-5.562 0-1.229.44-2.234 1.16-3.022-.117-.284-.503-1.43.11-2.98 0 0 .945-.303 3.094 1.154a10.78 10.78 0 0 1 5.634 0c2.148-1.457 3.092-1.154 3.092-1.154.615 1.55.229 2.696.113 2.98.722.788 1.158 1.793 1.158 3.022 0 4.324-2.63 5.275-5.136 5.553.404.35.764 1.043.764 2.1 0 1.517-.014 2.741-.014 3.113 0 .3.203.65.774.54A11.25 11.25 0 0 0 12 .75Z" />
        </svg>
      </a>
      <a href={`${repo}/releases`} target="_blank" rel="noopener noreferrer">
        {release?.version ?? 'v0.1.3'}
      </a>
      <a href="https://github.com/wanderson9" target="_blank" rel="noopener noreferrer">
        @wanderson9
      </a>
      <span>Built in Dana Point, CA 🇺🇸</span>
      {release?.updateAvailable && (
        <a
          className="update-available"
          href={`${repo}/releases`}
          target="_blank"
          rel="noopener noreferrer"
        >
          ⚑ Update available{release.latest ? `: ${release.latest}` : ''}
        </a>
      )}
    </footer>
  );
}
