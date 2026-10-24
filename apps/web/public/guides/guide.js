// Highlights the section in view in the table of contents, and enlarges a
// screenshot when it is clicked (Esc or a click closes it).
(function () {
    var links = [].slice.call(document.querySelectorAll('.toc a'));
    var secs = [].slice.call(document.querySelectorAll('.sec'));
    var ticking = false;
    function spy() {
        ticking = false;
        var cur = secs[0];
        secs.forEach(function (s) { if (s.getBoundingClientRect().top <= 150) cur = s; });
        // At the very bottom the last section may never reach the top.
        if (innerHeight + scrollY >= document.documentElement.scrollHeight - 4) cur = secs[secs.length - 1];
        links.forEach(function (a) { a.classList.toggle('on', cur && a.getAttribute('href') === '#' + cur.id); });
    }
    addEventListener('scroll', function () {
        if (!ticking) { ticking = true; requestAnimationFrame(spy); }
    }, {passive: true});
    spy();

    var zoom = document.createElement('div');
    zoom.className = 'zoom';
    document.body.appendChild(zoom);
    document.addEventListener('click', function (e) {
        var screen = e.target.closest && e.target.closest('.win-screen');
        if (screen && !zoom.contains(screen)) {
            var shot = screen.closest('.shot');
            zoom.innerHTML = '';
            zoom.appendChild(screen.closest('.win').cloneNode(true));
            zoom.style.setProperty('--sw', shot ? shot.style.getPropertyValue('--sw') : '800px');
            zoom.classList.add('on');
        } else if (zoom.classList.contains('on') && zoom.contains(e.target)) {
            zoom.classList.remove('on');
        }
    });
    addEventListener('keydown', function (e) {
        if (e.key === 'Escape') zoom.classList.remove('on');
    });
})();
