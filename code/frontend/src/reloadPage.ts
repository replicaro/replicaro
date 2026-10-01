// Its own module so tests can replace it: jsdom can't reload a page.
export function reloadPage(): void {
    window.location.reload();
}
