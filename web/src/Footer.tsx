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
      <div className="footer-release">
        <a href={`${repo}/releases`} target="_blank" rel="noopener noreferrer">
          ts-egress{release ? ` ${release.version}` : ''}
        </a>
        {release?.updateAvailable && (
          <a
            className="update-available"
            href={`${repo}/releases`}
            target="_blank"
            rel="noopener noreferrer"
          >
            Update available{release.latest ? `: ${release.latest}` : ''}
          </a>
        )}
      </div>
      <a href={repo} target="_blank" rel="noopener noreferrer" aria-label="ts-egress on GitHub">
        GitHub
      </a>
    </footer>
  );
}
