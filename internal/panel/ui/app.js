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
    announce(copied ? "Copied to clipboard" : "Copy unavailable. Select the text and copy it manually.");
  }
});

function announce(message) {
  clearTimeout(toastTimeout);
  toast.textContent = message;
  toast.hidden = false;
  toastTimeout = setTimeout(() => { toast.hidden = true; }, 2800);
}

const operatorRows = document.getElementById("operator-rows");
if (operatorRows) {
  const addOperator = document.querySelector("[data-add-operator]");
  const template = document.getElementById("operator-row-template");

  function updateOperatorLimit() {
    addOperator.disabled = operatorRows.children.length >= Number(operatorRows.dataset.limit);
  }

  addOperator.addEventListener("click", () => {
    if (addOperator.disabled) return;
    operatorRows.append(template.content.cloneNode(true));
    operatorRows.lastElementChild.querySelector("input").focus();
    updateOperatorLimit();
  });

  operatorRows.addEventListener("click", (event) => {
    const remove = event.target.closest("[data-remove-operator]");
    if (!remove) return;
    remove.closest(".operator-row").remove();
    updateOperatorLimit();
    addOperator.focus();
  });

  updateOperatorLimit();
}

const clientGrid = document.querySelector(".client-grid");
if (clientGrid) {
  const platform = navigator.userAgentData?.platform || navigator.platform || "";
  const agent = navigator.userAgent;
  let client;
  if (/Android/i.test(agent + platform)) {
    client = "v2rayng";
  } else if (/iPad|iPhone|iPod/i.test(agent + platform) ||
             (/Mac/i.test(platform) && navigator.maxTouchPoints > 1)) {
    client = "hiddify";
  } else if (/Win/i.test(platform) || /Windows/i.test(agent)) {
    client = "v2rayn";
  } else if (/Mac|Linux/i.test(platform)) {
    client = "hiddify";
  }
  const recommendation = clientGrid.querySelector(`[data-client="${client}"]`);
  if (recommendation) {
    recommendation.querySelector(".recommendation").hidden = false;
    clientGrid.prepend(recommendation);
  }
}
