const btn = document.getElementById('create');
const err = document.getElementById('error');
btn.addEventListener('click', async () => {
  btn.disabled = true;
  err.textContent = '';
  try {
    const res = await fetch('/api/rooms', { method: 'POST' });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || res.status);
    location.href = `/r/${data.id}`;
  } catch (e) {
    err.textContent = e.message;
    btn.disabled = false;
  }
});
