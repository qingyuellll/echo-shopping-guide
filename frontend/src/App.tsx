import { useEffect, useState } from 'react';
import { GuidePage } from './pages/GuidePage';
import { GuideAdminPage } from './pages/GuideAdminPage';

export function App() {
  const [managing, setManaging] = useState(() => window.location.hash === '#admin');

  useEffect(() => {
    const sync = () => {
      const hash = window.location.hash;
      // 管理端内部用 #ga-* 锚点做板块跳转，不能因为 hash 变化就退出管理端。
      if (hash.startsWith('#ga-')) return;
      setManaging(hash === '#admin');
    };
    window.addEventListener('hashchange', sync);
    return () => window.removeEventListener('hashchange', sync);
  }, []);

  function openAdmin() {
    window.location.hash = '#admin';
    setManaging(true);
  }

  function closeAdmin() {
    if (window.location.hash) window.location.hash = '';
    setManaging(false);
  }

  if (!managing) return <GuidePage onManage={openAdmin} />;
  return <GuideAdminPage onExit={closeAdmin} />;
}
