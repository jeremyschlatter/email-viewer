function redirectPOST(page, args) {
    var inputs = "";
    for (arg in args) {
        inputs += '<input type="text" name="' + arg + '" value="' + args[arg] + '" />';
    }
    var form = $('<form action="' + page + '" method="post">' + inputs + '</form>');
    $(form).submit();
}

function authorize(immediate, callback) {
    gapi.auth.authorize({client_id: '1057206095862-hdrc2h81rh8ecbtnep68c7e7k65bir21.apps.googleusercontent.com',
                         scope: 'https://mail.google.com/ https://www.googleapis.com/auth/userinfo.email',
                         immediate: immediate},
                        callback);
}

function handleAuthResult(authResult) {
    if (authResult && !authResult.error) {
        $.ajax({url: 'https://www.googleapis.com/userinfo/email?alt=json&oauth_token=' + authResult.access_token,
                success: function(result) {
                    redirectPOST('', { user: result.data.email, token: authResult.access_token });
                }});
    } else {
        var authButton = document.getElementById('authorize-button');
        authButton.onclick = function() {authorize(false, handleAuthResult);};
        authButton.style.visibility = '';
    }
}


function doit() {
    authorize(true, handleAuthResult);
}
