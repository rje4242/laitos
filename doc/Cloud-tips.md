# Cloud tips

## General information
laitos runs well on all popular cloud vendors such as Amazon Web Service, Microsoft Azure, and Google Compute Engine.

In fact, it works well on nearly all public and private cloud vendors, irrespective of computer form factor,
virtualisation technology, hardware model, and administration interface.

## Important note on sending outgoing mails
As an anti-spam measure, nearly all major public cloud vendors block outgoing contact to port 25, which means applications
running on their infrastructure will not be able to deliver outgoing mails - this does not interfere with mails coming in.
Therefore, use laitos program with a dedicated mail delivery services such as [sendgrid](https://sendgrid.com/) that has
anti-spam measures built-in, sendgrid accepts incoming connection on port 2525 for mail delivery.

In laitos system maintenance daemon configuration, if there is a connectivity check for port 25 on a foreign host, laitos
will skip that check.

## Start laitos automatically via systemd
systemd is the most popular init system for Linux distributions, it can help launching laitos automatically when
computer boots up. Create a service file `/etc/systemd/system/laitos.service` and write:

    [Unit]
    Description=laitos - personal Internet infrastructure
    After=network.target
    
    [Service]
    ExecStart=/root/laitos/laitos -disableconflicts -gomaxprocs 8 -config config.json -daemons autounlock,dnsd,httpd,insecurehttpd,maintenance,phonehome,plainsocket,simpleipsvcd,smtpd,snmpd,sockd,telegram
    User=root
    Group=root
    WorkingDirectory=/root/laitos
    PrivateTmp=true
    RestartSec=3600
    Restart=always
    
    [Install]
    WantedBy=multi-user.target

Make sure to alter the paths in `ExecStart` and `WorkingDirectory` according to your setup. For security, please place
laitos program (e.g. `/root/laitos/laitos`) and its data directory (e.g. `/root/laitos`) in a location accessible only
by superuser `root`.

After the service file is in place, run these commands as root user:

    # systemctl daemon-reload    (Reload daemon service files, including the new one for laitos.)
    # systemctl enable laitos    (Automatically start laitos when system boots up)
    # systemctl start laitos     (Start laitos right now)

## Deploy on Amazon Web Service - EC2
Simply copy laitos program and its data onto an EC2 instance, compose and save the system service file, start laitos
right away. All flavours of Linux distributions that run on EC2 can run laitos.

## Deploy on Amazon Web Service - Lambda
Lambda is the flagship Function-as-a-Service product offered by AWS. As a serverless offering, the product does not support
general purpose computing. For laitos, lambda can run its web server without having to prepare an EC2 instance manually.

Lambda offers pre-built runtime environment (including Go) with built-in web server. Because laitos runs its own web server,
we will build a "lambda custom runtime", start with creating a shell script and name it `bootstrap`:

    #!/bin/sh
    ./laitos -config config.json -daemons insecurehttpd -awslambda

Note that:

1. The script file must be named `bootstrap` without an extension name. The file must have executable permission: `chmod 755 bootstrap`.
2. If your laitos data files, such as web pages, static assets, and configuration files reside in a sub-directory, then put
   an additional statement `cd MyLaitosDataDirectory || exit 1` before the statement executing laitos program.
3. Lambda uses a custom domain name provided by AWS to serve websites, therefore laitos needs to run the HTTP web server
   `insecurehttpd` rather than the HTTPS web server `httpd`.
4. Lambda is not a general purpose computing service, it is unable to work with other laitos daemons such as `dnsd` and
   `smtpd`, it is harmless to include them in the `-daemons` list, though doing so will slow down lambda.

Zip the compiled laitos program, program data files, and the important `bootstrap` file:

    zip -r my-laitos-bundle.zip ./bootstrap ./laitos ./MyLaitosDataDirectory

Visit AWS management console and create a lambda function:

1. Choose "Author from scratch" option for creating the new function.
2. Name the function however you wish.
3. Choose "Provide your own bootstrap" as the function runtime.

AWS management console does not seem to let us upload the zip file, so we have to use AWS CLI:

    aws lambda update-function-code --function-name my-lambda-function-name --zip-file fileb://my-laitos-bundle.zip

Check out AWS official guide [Custom Runtime](https://docs.aws.amazon.com/lambda/latest/dg/runtimes-walkthrough.html) for
more help.

Finally, create an AWS API Gateway (serverless web middleware) to expose laitos lambda function in an AWS-managed server:

1. Choose "REST API" for the API type. Do not use "HTTP API" or "REST API private".
2. Choose "REST" for the protocol and "regional" for the endpoint type.
3. Create an API resource "/", matching any method, in Integration Request choose "Lambda function" and enable
   "Use Lambda proxy integration", select your laitos lambda function. Leave other request and response options at default.
4. Create an API resource "/{proxy+}", matching any method, configure Integration Request in the same way.
5. Navigate to API Settings, find "Binary Media Types" and enter "*/*" without quotes as the binary media type.
6. Deploy the API to a Stage.
7. Navigate to stage's URL in web browser, it should greet with your laitos web server home page.

If something seems amiss, enable CloudWatch logging in Stage editor, and navigate to CloudWatch console to find both
API gateway and Lambda log streams, they may give a clue.

## Deploy on Amazon Web Service - Elastic Beanstalk
AWS offers a Platform-as-a-Service product "ElasticBeanstalk" that automatically manages EC2 instances for you.
Here are some tips for using laitos on ElasticBeanstalk:
- For a personal web server, it is sufficient to use "Single Instance" as environment type. Load balancer incurs
  additional cost and it is often not necessary for a personal web server.
- ElasticBeanstalk always expects their application to serve a web server on port 5000, otherwise it will consider
  an application (laitos) to be malfunctioning. If your laitos configuration has already set up web server, you must
  start `insecurehttpd` that will automatically listen on port 5000 when it detects ElasticBeanstalk environment.
  ElasticBeanstalk system will then serve (proxy) Internet visitors on port 80 using laitos web server on port 5000.
- If your configuration does not set up a web server, please set one up just for ElasticBeanstalk, or use the ordinary
  deployment method.
- ElasticBeanstalk application is packed into a zip file called "application bundle", that typically contains:

      Procfile                       (launch command line text)
      .ebextensions/options.config   (workaround - launch laitos as root)

      laitos       (program executable)
      config.json  (configuration file)

      index.html   (other data)
      ...

- `Procfile` tells the command line for starting laitos, the content may look like:

      laitos: ./laitos -disableconflicts -gomaxprocs 16 -config config.json -daemons dnsd,httpd,insecurehttpd,maintenance,plainsocket,simpleipsvcd,smtpd,snmpd,sockd,telegram

  Be aware that, paths among the command line must be relative to the top level of application bundle zip file.

- `.ebextensions/options.config` alters operating system configuration in order to launch laitos as user root.
  Otherwise ElasticBeanstalk launches program using an unprivileged user that will cause laitos to malfunction.
  The file shall contain the following content:

      ---
      commands:
        0_run_as_root:
          command: "find /opt/elasticbeanstalk -name 'app.conf.erb' -exec sed -i 's/^user=.*$/user=root/' {} \\;"
        1_reload_supervisor:
          command: "/usr/local/bin/supervisorctl -c /etc/supervisor/supervisord.conf reload"
          ignoreErrors: true

- Remember to adjust firewall (security group) to open ports for all services (e.g. DNS, SMTP, HTTPS) served by laitos.

## Deploy on Microsoft Azure and Google Compute Engine
Simply copy laitos program and its data onto a Linux virtual machine and start laitos right away. It is often useful to
use systemd integration to launch laitos automatically upon system boot. All flavours of Linux distributions supported
by Azure can run laitos.

## Deploy on other cloud providers
laitos runs on nearly all flavours of Linux system, therefore as long as your cloud provider supports Linux compute
instance, you can be almost certain that it will run laitos smoothly and well.

Beyond AWS, Azure, and GCE, the author of laitos has also successfully deployed it on generic KVM virtual machine,
OpenStack, Linode, and several cheap hosting services advertised on [lowendbox.com](https://lowendbox.com/).
