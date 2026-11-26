const toast = document.querySelector(".toast");
let toastTimeout;

document.addEventListener("click", async (event) => {
  const opener = event.target.closest("[data-dialog]");
  if (opener) {
    const dialog = document.getElementById(opener.dataset.dialog);
    if (dialog?.showModal) {
      event.preventDefault();
      dialog.showModal();
    }
  }

  const copy = event.target.closest("[data-copy]");
  if (!copy) return;
  const value = copy.dataset.copy;
  try {
    await navigator.clipboard.writeText(value);
    announce("Copied to clipboard");
  } catch {
    const field = document.createElement("textarea");
    field.className = "visually-hidden";
    field.value = value;
    document.body.append(field);
    field.select();
    const copied = document.execCommand("copy");
    field.remove();
    copy.focus();
    announce(copied ? "Copied to clipboard" : "Copy unavailable. Select the link on the account page.");
  }
});

function announce(message) {
  clearTimeout(toastTimeout);
  toast.textContent = message;
  toast.hidden = false;
  toastTimeout = setTimeout(() => { toast.hidden = true; }, 2800);
}
