var sanitizer = require('./html-sanitizer.js');

var input = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', function(chunk) {
    input += chunk;
});
process.stdin.on('end', function() {
    process.stdout.write(sanitizer.sanitize(input, function(url) { return url }));
});
